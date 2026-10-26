use super::*;
use axum::{Router, routing::get};
use futures::StreamExt;
use std::convert::Infallible;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

const DELTA: &str = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"test\"}\n\n";
const COMPLETED: &str = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[]}}\n\n";

async fn assert_active_stream(idle_timeout: Duration, interval: Duration, count: usize) {
    let upstream_listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let upstream_address = upstream_listener.local_addr().unwrap();
    let upstream = Router::new().route(
        "/responses",
        get(move || async move {
            let stream = futures::stream::unfold(0, move |index| async move {
                if index > count {
                    return None;
                }
                tokio::time::sleep(interval).await;
                let data = if index == count { COMPLETED } else { DELTA };
                Some((
                    Ok::<_, Infallible>(Bytes::from_static(data.as_bytes())),
                    index + 1,
                ))
            });
            Response::builder()
                .header("content-type", "text/event-stream")
                .header("x-test", "preserved")
                .body(Body::from_stream(stream))
                .unwrap()
        }),
    );
    let upstream_task = tokio::spawn(async move {
        axum::serve(upstream_listener, upstream).await.unwrap();
    });

    let client = upstream_client_builder(idle_timeout)
        .no_proxy()
        .build()
        .unwrap();
    let proxy = Router::new().route(
        "/v1/responses",
        get(move || {
            let client = client.clone();
            async move {
                forward_response(
                    client
                        .get(format!("http://{upstream_address}/responses"))
                        .send()
                        .await?,
                )
                .await
            }
        }),
    );
    let proxy_listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let proxy_address = proxy_listener.local_addr().unwrap();
    let proxy_task = tokio::spawn(async move {
        axum::serve(proxy_listener, proxy).await.unwrap();
    });

    let started = Instant::now();
    let result = tokio::time::timeout(idle_timeout + interval * (count as u32 + 10), async {
        let response = Client::builder().no_proxy().build().unwrap()
            .get(format!("http://{proxy_address}/v1/responses")).send().await.unwrap();
        assert_eq!(response.status(), reqwest::StatusCode::OK);
        assert_eq!(response.headers()["x-test"], "preserved");
        assert_eq!(response.headers()["cache-control"], "no-cache");
        let mut stream = response.bytes_stream();
        let mut body = Vec::new();
        let mut chunks = 0;
        while let Some(chunk) = stream.next().await {
            body.extend_from_slice(&chunk.unwrap());
            chunks += 1;
        }
        assert_eq!(body, format!("{}{COMPLETED}", DELTA.repeat(count)).as_bytes());
        assert!(started.elapsed() > idle_timeout);
        println!("active SSE: elapsed={:?}, chunks={chunks}, bytes={}, response.completed preserved; upstream={upstream_address}, proxy={proxy_address}", started.elapsed(), body.len());
    }).await;
    proxy_task.abort();
    upstream_task.abort();
    result.unwrap();
}

#[tokio::test]
async fn active_sse_outlives_timeout_and_preserves_completion() {
    assert_active_stream(Duration::from_millis(500), Duration::from_millis(100), 12).await;
}

#[tokio::test]
#[ignore = "real-time 310 second isolated SSE regression"]
async fn active_sse_outlives_300_seconds() {
    assert_active_stream(UPSTREAM_IDLE_TIMEOUT, Duration::from_secs(1), 309).await;
}

async fn stalled_server(wire: &'static [u8]) -> (String, tokio::task::JoinHandle<()>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let mut request = vec![0; 16384];
        assert!(socket.read(&mut request).await.unwrap() > 0);
        socket.write_all(wire).await.unwrap();
        std::future::pending::<()>().await;
    });
    (address.to_string(), task)
}

#[tokio::test]
async fn idle_response_headers_time_out() {
    let idle = Duration::from_millis(150);
    let (address, server) = stalled_server(b"").await;
    let result = tokio::time::timeout(Duration::from_secs(3), async {
        upstream_client_builder(idle)
            .no_proxy()
            .build()
            .unwrap()
            .get(format!("http://{address}"))
            .send()
            .await
            .unwrap_err()
    })
    .await;
    server.abort();
    assert!(result.unwrap().is_timeout());
}

#[tokio::test]
async fn idle_sse_body_times_out_after_data() {
    let idle = Duration::from_millis(150);
    let (address, server) = stalled_server(b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\nb\r\ndata: test\n\r\n").await;
    let result = tokio::time::timeout(Duration::from_secs(3), async {
        let mut response = upstream_client_builder(idle)
            .no_proxy()
            .build()
            .unwrap()
            .get(format!("http://{address}"))
            .send()
            .await
            .unwrap();
        assert_eq!(response.chunk().await.unwrap().unwrap(), "data: test\n");
        response.chunk().await.unwrap_err()
    })
    .await;
    server.abort();
    assert!(result.unwrap().is_timeout());
}

#[tokio::test]
async fn stalled_tls_connection_times_out() {
    let (address, server) = stalled_server(b"").await;
    let started = Instant::now();
    let result = tokio::time::timeout(Duration::from_secs(3), async {
        upstream_client_builder(Duration::from_millis(150))
            .read_timeout(Duration::from_secs(10))
            .no_proxy()
            .build()
            .unwrap()
            .get(format!("https://{address}"))
            .send()
            .await
            .unwrap_err()
    })
    .await;
    server.abort();
    let error = result.unwrap();
    assert!(error.is_timeout());
    assert!(error.is_connect());
    assert!(started.elapsed() < Duration::from_secs(3));
}
