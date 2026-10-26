//! Axum server: router, handlers, and application state.

use crate::amp::AmpManagementProxy;
use crate::amp::local::LocalAmpState;
use crate::auth::TokenManager;
use crate::droid::DroidManagementProxy;
use crate::droid::local::LocalDroidState;
use crate::error::Error;
use crate::llm;
use crate::proxy::ProxyClient;
use crate::web_backend::SearchProvider;
use axum::Router;
use axum::body::Bytes;
use axum::extract::{DefaultBodyLimit, OriginalUri, Path, State};
use axum::http::Request;
use axum::http::{HeaderMap, Method};
use axum::middleware::{self, Next};
use axum::response::Response;
use axum::routing::{any, get};
use std::sync::Arc;
use tower_http::trace::MakeSpan;
use tracing::Instrument;

#[derive(Clone)]
pub struct AppState {
    pub(crate) proxy: Arc<ProxyClient>,
    pub(crate) amp_management: Arc<AmpManagementProxy>,
    pub(crate) amp_local: Option<Arc<LocalAmpState>>,
    pub(crate) droid_management: Arc<DroidManagementProxy>,
    pub(crate) droid_local: Option<Arc<LocalDroidState>>,
}

impl AppState {
    pub async fn new(
        amp_local: bool,
        droid_local: bool,
        search_provider: SearchProvider,
        search_model: Option<String>,
    ) -> Result<Self, Error> {
        let token = crate::config::load_github_token()?;
        let manager = Arc::new(TokenManager::new(token).await?);
        let proxy = Arc::new(ProxyClient::new(manager)?);
        let amp_management = Arc::new(AmpManagementProxy::new());
        let amp_local_state = if amp_local {
            Some(Arc::new(LocalAmpState::new(
                search_provider.clone(),
                Arc::clone(&proxy),
                search_model.clone(),
            )))
        } else {
            None
        };
        let droid_management = Arc::new(DroidManagementProxy::new());
        let droid_local_state = if droid_local {
            Some(Arc::new(LocalDroidState::new(
                search_provider,
                Arc::clone(&proxy),
                search_model,
            )))
        } else {
            None
        };
        Ok(Self {
            proxy,
            amp_management,
            amp_local: amp_local_state,
            droid_management,
            droid_local: droid_local_state,
        })
    }
}

/// Custom span factory that adds empty `initiator` and `upstream` fields to be
/// filled in later when the values are known.
#[derive(Clone)]
struct CopilotMakeSpan;

impl<B> MakeSpan<B> for CopilotMakeSpan {
    fn make_span(&mut self, request: &Request<B>) -> tracing::Span {
        tracing::info_span!(
            "request",
            method = %request.method(),
            uri = %request.uri(),
            initiator = tracing::field::Empty,
            upstream = tracing::field::Empty,
        )
    }
}

/// Record the resolved initiator and upstream path into the current request
/// span so they appear in the TraceLayer's response log line.
pub fn record_upstream(initiator: &str, path: &str) {
    let span = tracing::Span::current();
    span.record("initiator", initiator);
    span.record("upstream", path);
}

const MAX_REQUEST_BODY_BYTES: usize = 32 * 1024 * 1024;

fn ingress_route_label(path: &str) -> &'static str {
    match path {
        "/v1/responses" => "/v1/responses",
        "/v1/chat/completions" => "/v1/chat/completions",
        "/v1/messages" => "/v1/messages",
        "/v1/messages/count_tokens" => "/v1/messages/count_tokens",
        "/v1/models" => "/v1/models",
        _ => "other",
    }
}

async fn diagnose_request_body(request: Request<axum::body::Body>, next: Next) -> Response {
    let ingress_request_id = uuid::Uuid::new_v4().to_string();
    let route = ingress_route_label(request.uri().path());
    let declared_body_bytes = request
        .headers()
        .get(axum::http::header::CONTENT_LENGTH)
        .and_then(|value| value.to_str().ok()?.parse::<u64>().ok());
    let declared_over_limit =
        declared_body_bytes.is_some_and(|bytes| bytes > MAX_REQUEST_BODY_BYTES as u64);
    let started = std::time::Instant::now();
    // This span also correlates existing handler/upstream logs, without
    // forwarding an untrusted request ID or logging any raw headers/URI.
    let span = tracing::info_span!("ingress", ingress_request_id = %ingress_request_id);
    async move {
        tracing::info!(
            diagnostic = "request_body_ingress", route,
            declared_body_bytes = ?declared_body_bytes,
            body_length_known = declared_body_bytes.is_some(),
            body_limit_bytes = MAX_REQUEST_BODY_BYTES as u64,
            declared_over_limit,
            "Request reached proxy before body-limit enforcement"
        );
        // Do not read, clone, decompress, or transform the request body.
        let response = next.run(request).await;
        let status = response.status().as_u16();
        let returned_413 = status == 413;
        if returned_413 {
            tracing::warn!(
                diagnostic = "request_body_413", route, status,
                declared_body_bytes = ?declared_body_bytes,
                body_limit_bytes = MAX_REQUEST_BODY_BYTES as u64,
                declared_over_limit,
                elapsed_ms = started.elapsed().as_millis() as u64,
                "Payload-too-large response; local header-limit rejection established only when declared_over_limit=true"
            );
        } else {
            tracing::info!(
                diagnostic = "request_body_response_headers", route, status,
                elapsed_ms = started.elapsed().as_millis() as u64,
                "Proxy response headers ready (not a stream-completion assertion)"
            );
        }
        response
    }
    .instrument(span)
    .await
}

fn with_request_body_limits<S>(router: Router<S>) -> Router<S>
where
    S: Clone + Send + Sync + 'static,
{
    // The outer limit does not override Bytes/Json's default 2 MiB limit.
    // Keep extraction and streaming limits aligned and bounded.
    router
        .layer(DefaultBodyLimit::max(MAX_REQUEST_BODY_BYTES))
        .layer(tower_http::limit::RequestBodyLimitLayer::new(
            MAX_REQUEST_BODY_BYTES,
        ))
        // Axum's last-added layer runs first, including on an early 413.
        .layer(middleware::from_fn(diagnose_request_body))
}

pub fn create_router(state: AppState) -> Router {
    with_request_body_limits(
        Router::new()
            .route("/v1/usage", get(usage_handler))
            .route("/v1/{*path}", any(proxy_handler))
            .merge(crate::api::routes())
            .merge(crate::amp::root_routes())
            .layer(tower_http::trace::TraceLayer::new_for_http().make_span_with(CopilotMakeSpan)),
    )
    .with_state(state)
}

async fn usage_handler(State(state): State<AppState>) -> Result<Response, Error> {
    let resp = state.proxy.fetch_usage().await?;
    crate::proxy::forward_response(resp).await
}

async fn proxy_handler(
    State(state): State<AppState>,
    method: Method,
    Path(path): Path<String>,
    OriginalUri(uri): OriginalUri,
    headers: HeaderMap,
    body: Bytes,
) -> Result<Response, Error> {
    if path == "messages" || path == "messages/count_tokens" {
        return llm::handle_anthropic_compat(
            &state,
            method,
            &path,
            uri.query(),
            &headers,
            body,
            true,
        )
        .await;
    }

    llm::handle_openai_passthrough(&state, method, &path, uri.query(), &headers, body).await
}

#[cfg(test)]
mod body_limit_tests {
    use super::*;
    use axum::body::{Body, to_bytes};
    use axum::http::StatusCode;
    use std::convert::Infallible;
    use tower::ServiceExt;

    async fn check_boundary(size: usize, chunked: bool) {
        // Echo Bytes so both the extraction limit and byte preservation are tested.
        let app = with_request_body_limits(Router::new().route(
            "/v1/chat/completions",
            any(|body: Bytes| async move { body }),
        ));
        let payload = vec![b'x'; size];
        let body = if chunked {
            let chunks: Vec<_> = payload
                .chunks(64 * 1024)
                .map(|chunk| Ok::<_, Infallible>(Bytes::copy_from_slice(chunk)))
                .collect();
            Body::from_stream(futures::stream::iter(chunks))
        } else {
            Body::from(payload.clone())
        };
        let mut request = Request::builder()
            .method(Method::POST)
            .uri("/v1/chat/completions");
        if !chunked {
            request = request.header("content-length", size);
        }
        let response = app.oneshot(request.body(body).unwrap()).await.unwrap();
        if size > MAX_REQUEST_BODY_BYTES {
            assert_eq!(response.status(), StatusCode::PAYLOAD_TOO_LARGE);
        } else {
            assert_eq!(response.status(), StatusCode::OK);
            let echoed = to_bytes(response.into_body(), MAX_REQUEST_BODY_BYTES)
                .await
                .unwrap();
            assert_eq!(echoed.as_ref(), payload.as_slice());
        }
    }

    #[tokio::test]
    async fn known_length_request_body_boundaries() {
        for size in [
            2 * 1024 * 1024 + 1,
            12_243_726, // Regression: the Desktop request rejected by the former 10 MiB cap.
            MAX_REQUEST_BODY_BYTES - 1,
            MAX_REQUEST_BODY_BYTES,
            MAX_REQUEST_BODY_BYTES + 1,
        ] {
            check_boundary(size, false).await;
        }
    }

    #[tokio::test]
    async fn unknown_length_request_body_boundaries() {
        for size in [
            2 * 1024 * 1024 + 1,
            12_243_726,
            MAX_REQUEST_BODY_BYTES - 1,
            MAX_REQUEST_BODY_BYTES,
            MAX_REQUEST_BODY_BYTES + 1,
        ] {
            check_boundary(size, true).await;
        }
    }

    #[tokio::test]
    async fn ingress_logs_early_rejection_without_sensitive_values() {
        use std::io::Write;
        use std::sync::Mutex;
        use tracing::instrument::WithSubscriber;

        #[derive(Clone)]
        struct LogWriter(Arc<Mutex<Vec<u8>>>);
        impl Write for LogWriter {
            fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
                self.0.lock().unwrap().extend_from_slice(bytes);
                Ok(bytes.len())
            }
            fn flush(&mut self) -> std::io::Result<()> {
                Ok(())
            }
        }
        let captured = Arc::new(Mutex::new(Vec::new()));
        let writer = LogWriter(Arc::clone(&captured));
        let subscriber = tracing_subscriber::fmt()
            .without_time()
            .with_ansi(false)
            .with_writer(move || writer.clone())
            .finish();
        async fn must_not_run() -> &'static str {
            panic!("oversized request reached handler")
        }
        let app = with_request_body_limits(Router::new().route("/v1/responses", any(must_not_run)));
        let request = Request::builder()
            .uri("/v1/responses?api_key=QUERY_SECRET")
            .header("authorization", "Bearer AUTH_SECRET")
            .header("x-request-id", "CLIENT_SECRET")
            .header("content-length", MAX_REQUEST_BODY_BYTES + 42)
            .body(Body::from("BODY_SECRET"))
            .unwrap();
        let response = app
            .oneshot(request)
            .with_subscriber(subscriber)
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::PAYLOAD_TOO_LARGE);
        let logs = String::from_utf8(captured.lock().unwrap().clone()).unwrap();
        assert!(logs.contains("request_body_ingress"));
        assert!(logs.contains("request_body_413"));
        assert!(logs.contains("ingress_request_id="));
        assert!(logs.contains(&format!("Some({})", MAX_REQUEST_BODY_BYTES + 42)));
        assert!(logs.contains("declared_over_limit=true"));
        for secret in [
            "QUERY_SECRET",
            "AUTH_SECRET",
            "CLIENT_SECRET",
            "BODY_SECRET",
        ] {
            assert!(!logs.contains(secret));
        }
    }

    #[test]
    fn ingress_route_labels_do_not_log_arbitrary_paths() {
        assert_eq!(ingress_route_label("/private/SECRET"), "other");
        assert_eq!(ingress_route_label("/v1/responses"), "/v1/responses");
    }
}
