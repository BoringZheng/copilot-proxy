//! HTTP proxy client for forwarding requests to Copilot API.

use crate::auth::TokenManager;
use crate::error::Error;
use axum::body::{Body, Bytes};
use axum::response::Response;
use futures::{StreamExt, TryStreamExt};
use reqwest::Client;
use reqwest::header::{HeaderMap, HeaderValue};
use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::sync::RwLock;

const HOP_BY_HOP: &[&str] = &[
    "transfer-encoding",
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailers",
    "upgrade",
];

const MODELS_CACHE_TTL: Duration = Duration::from_secs(24 * 60 * 60);
const MODELS_UNKNOWN_REFRESH_TTL: Duration = Duration::from_secs(5 * 60);

#[derive(Clone)]
struct UpstreamAttempt {
    request_id: String,
    route: &'static str,
    started: Instant,
}

impl UpstreamAttempt {
    fn new(path: &str) -> Self {
        // Never log arbitrary paths or query strings: these may contain secrets.
        let route = match path.split('?').next().unwrap_or("") {
            "/chat/completions" => "/chat/completions",
            "/responses" => "/responses",
            "/models" => "/models",
            "/v1/messages" => "/v1/messages",
            _ => "other",
        };
        Self {
            request_id: uuid::Uuid::new_v4().to_string(),
            route,
            started: Instant::now(),
        }
    }
}

// Deliberately log only allowlisted classifications, never Display/Debug on
// arbitrary errors: URLs, tokens, and payload fragments can occur in them.
struct TransportFailure {
    timeout: bool,
    connect: bool,
    body: bool,
    decode: bool,
    causes: Vec<&'static str>,
    io_kind: Option<std::io::ErrorKind>,
    os_error: Option<i32>,
}

impl std::fmt::Debug for TransportFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TransportFailure")
            .field("timeout", &self.timeout)
            .field("connect", &self.connect)
            .field("body", &self.body)
            .field("decode", &self.decode)
            .field("causes", &self.causes)
            .field("io_kind", &self.io_kind)
            .field("os_error", &self.os_error)
            .finish()
    }
}

impl TransportFailure {
    fn classify(error: &reqwest::Error) -> Self {
        let mut result = Self {
            timeout: error.is_timeout(),
            connect: error.is_connect(),
            body: error.is_body(),
            decode: error.is_decode(),
            causes: Vec::new(),
            io_kind: None,
            os_error: None,
        };
        let mut source: Option<&(dyn std::error::Error + 'static)> = Some(error);
        for _ in 0..16 {
            let Some(current) = source else { break };
            if let Some(io) = current.downcast_ref::<std::io::Error>() {
                result.io_kind = Some(io.kind());
                result.os_error = io.raw_os_error();
            }
            let message = current.to_string().to_ascii_lowercase();
            for (pattern, label) in [
                ("connection reset", "connection_reset"),
                ("connection refused", "connection_refused"),
                ("broken pipe", "broken_pipe"),
                ("unexpected eof", "unexpected_eof"),
                ("incomplete message", "incomplete_message"),
                ("end of file", "unexpected_eof"),
                ("timed out", "timeout"),
                ("timeout", "timeout"),
                ("certificate", "tls_certificate"),
                ("tls", "tls"),
                ("dns", "dns"),
                ("name resolution", "dns"),
                ("lookup address", "dns"),
                ("http2", "http2"),
                ("http/2", "http2"),
                ("goaway", "http2_goaway"),
                ("go_away", "http2_goaway"),
                ("refused_stream", "http2_refused_stream"),
                ("cancel", "cancelled"),
                ("connection closed", "connection_closed"),
                ("connection aborted", "connection_aborted"),
            ] {
                if message.contains(pattern) && !result.causes.contains(&label) {
                    result.causes.push(label);
                }
            }
            source = current.source();
        }
        result
    }
}

struct BodyDiagnostic {
    attempt: UpstreamAttempt,
    headers_at: Instant,
    last_chunk: Instant,
    chunks: u64,
    bytes: u64,
    finished: bool,
}

impl BodyDiagnostic {
    fn new(attempt: UpstreamAttempt) -> Self {
        let now = Instant::now();
        Self {
            attempt,
            headers_at: now,
            last_chunk: now,
            chunks: 0,
            bytes: 0,
            finished: false,
        }
    }

    fn chunk(&mut self, bytes: usize) {
        self.chunks += 1;
        self.bytes += bytes as u64;
        self.last_chunk = Instant::now();
    }

    fn finish(&mut self, error: Option<&reqwest::Error>) {
        self.finished = true;
        if let Some(error) = error {
            tracing::warn!(
                diagnostic = "upstream_body_error",
                request_id = %self.attempt.request_id, route = self.attempt.route,
                elapsed_ms = self.attempt.started.elapsed().as_millis() as u64,
                body_ms = self.headers_at.elapsed().as_millis() as u64,
                idle_ms = self.last_chunk.elapsed().as_millis() as u64,
                received_chunks = self.chunks, received_bytes = self.bytes,
                failure = ?TransportFailure::classify(error),
                "Copilot response body failed"
            );
        } else {
            tracing::info!(
                diagnostic = "upstream_body_eof",
                request_id = %self.attempt.request_id, route = self.attempt.route,
                elapsed_ms = self.attempt.started.elapsed().as_millis() as u64,
                received_chunks = self.chunks, received_bytes = self.bytes,
                "Copilot transport reached EOF (not a model completion assertion)"
            );
        }
    }
}

impl Drop for BodyDiagnostic {
    fn drop(&mut self) {
        if !self.finished {
            tracing::info!(
                diagnostic = "upstream_body_dropped",
                request_id = %self.attempt.request_id, route = self.attempt.route,
                elapsed_ms = self.attempt.started.elapsed().as_millis() as u64,
                received_chunks = self.chunks, received_bytes = self.bytes,
                "Response consumer dropped body before transport EOF; upstream failure not established"
            );
        }
    }
}

fn diagnostic_bytes_stream(
    resp: reqwest::Response,
    diagnostic: BodyDiagnostic,
) -> impl futures::Stream<Item = Result<Bytes, reqwest::Error>> + Send {
    futures::stream::unfold(
        (Box::pin(resp.bytes_stream()), diagnostic),
        |(mut upstream, mut diagnostic)| async move {
            if diagnostic.finished {
                return None;
            }
            match upstream.next().await {
                Some(Ok(chunk)) => {
                    diagnostic.chunk(chunk.len());
                    Some((Ok(chunk), (upstream, diagnostic)))
                }
                Some(Err(error)) => {
                    diagnostic.finish(Some(&error));
                    Some((Err(error), (upstream, diagnostic)))
                }
                None => {
                    diagnostic.finish(None);
                    None
                }
            }
        },
    )
}

const UPSTREAM_IDLE_TIMEOUT: Duration = Duration::from_secs(300);

fn upstream_client_builder(idle_timeout: Duration) -> reqwest::ClientBuilder {
    // A total timeout also cuts off healthy SSE streams while data is arriving.
    Client::builder()
        .connect_timeout(idle_timeout)
        .read_timeout(idle_timeout)
}

struct ModelsCache {
    fetched_at: Instant,
    reasoning_efforts_by_model: HashMap<String, Vec<String>>,
}

pub struct ProxyClient {
    client: Client,
    token_manager: Arc<TokenManager>,
    device_id: String,
    machine_id: String,
    session_id: String,
    models_cache: RwLock<Option<ModelsCache>>,
}

impl ProxyClient {
    pub fn new(token_manager: Arc<TokenManager>) -> Result<Self, Error> {
        let client = upstream_client_builder(UPSTREAM_IDLE_TIMEOUT).build()?;
        let device_id = crate::config::load_vscode_device_id();
        let machine_id = crate::config::load_vscode_machine_id();
        let session_id = uuid::Uuid::new_v4().to_string();
        Ok(Self {
            client,
            token_manager,
            device_id,
            machine_id,
            session_id,
            models_cache: RwLock::new(None),
        })
    }

    pub async fn forward(
        &self,
        path: &str,
        method: reqwest::Method,
        body: Bytes,
        content_type: Option<&str>,
        initiator: Option<&str>,
        is_vision: bool,
    ) -> Result<reqwest::Response, Error> {
        self.forward_inner(path, method, body, content_type, initiator, is_vision, true)
            .await
    }

    async fn forward_inner(
        &self,
        path: &str,
        method: reqwest::Method,
        body: Bytes,
        content_type: Option<&str>,
        initiator: Option<&str>,
        is_vision: bool,
        track_upstream: bool,
    ) -> Result<reqwest::Response, Error> {
        let token = self.token_manager.get_token().await?;
        let api_base = self.token_manager.get_api_base().await?;

        let resp = self
            .send_request(
                &api_base,
                path,
                method.clone(),
                &body,
                content_type,
                &token,
                initiator,
                is_vision,
                track_upstream,
            )
            .await?;

        // On 401, force-refresh the Copilot token and retry once (handles sleep/wake expiry)
        if resp.status() == reqwest::StatusCode::UNAUTHORIZED {
            tracing::warn!("Received 401 from upstream, attempting token refresh and retry");
            if self.token_manager.force_refresh(&token).await.is_ok() {
                let new_token = self.token_manager.get_token().await?;
                let new_api_base = self.token_manager.get_api_base().await?;
                return self
                    .send_request(
                        &new_api_base,
                        path,
                        method,
                        &body,
                        content_type,
                        &new_token,
                        initiator,
                        is_vision,
                        track_upstream,
                    )
                    .await;
            }
        }

        Ok(resp)
    }

    pub async fn fetch_usage(&self) -> Result<reqwest::Response, Error> {
        let github_token = &self.token_manager.github_token;
        let resp = self
            .client
            .get("https://api.github.com/copilot_internal/user")
            .header("Authorization", format!("token {}", github_token))
            .header("Accept", "application/json")
            .header("editor-version", "vscode/1.114.0")
            .header("editor-plugin-version", "copilot-chat/0.26.7")
            .header("user-agent", "GitHubCopilotChat/0.26.7")
            .header("x-github-api-version", "2026-01-09")
            .send()
            .await?;
        Ok(resp)
    }

    pub async fn supported_reasoning_efforts(
        &self,
        model: &str,
    ) -> Result<Option<Vec<String>>, Error> {
        let keys = model_lookup_keys(model);
        if let Some(efforts) = self.cached_reasoning_efforts(&keys).await {
            return Ok(efforts);
        }

        let reasoning_efforts_by_model = self.fetch_model_reasoning_efforts().await?;
        let efforts = keys
            .iter()
            .find_map(|key| reasoning_efforts_by_model.get(key).cloned());
        *self.models_cache.write().await = Some(ModelsCache {
            fetched_at: Instant::now(),
            reasoning_efforts_by_model,
        });
        Ok(efforts)
    }

    async fn cached_reasoning_efforts(&self, keys: &[String]) -> Option<Option<Vec<String>>> {
        let cache = self.models_cache.read().await;
        let cache = cache.as_ref()?;
        cached_reasoning_efforts_from_cache(cache, keys)
    }

    async fn fetch_model_reasoning_efforts(&self) -> Result<HashMap<String, Vec<String>>, Error> {
        let resp = self
            .forward_inner(
                "/models",
                reqwest::Method::GET,
                Bytes::new(),
                None,
                None,
                false,
                false,
            )
            .await?;
        if !resp.status().is_success() {
            return Err(Error::InvalidRequest(format!(
                "failed to fetch Copilot models: HTTP {}",
                resp.status()
            )));
        }

        let value: serde_json::Value = resp.json().await?;
        let mut map = HashMap::new();
        let Some(models) = value.get("data").and_then(|v| v.as_array()) else {
            return Ok(map);
        };

        for model in models {
            let Some(efforts) = model
                .pointer("/capabilities/supports/reasoning_effort")
                .and_then(|v| v.as_array())
                .map(|values| {
                    values
                        .iter()
                        .filter_map(|v| v.as_str().map(|s| s.to_string()))
                        .collect::<Vec<_>>()
                })
                .filter(|values| !values.is_empty())
            else {
                continue;
            };

            for pointer in ["/id", "/version", "/capabilities/family"] {
                if let Some(key) = model.pointer(pointer).and_then(|v| v.as_str()) {
                    for key in model_lookup_keys(key) {
                        map.insert(key, efforts.clone());
                    }
                }
            }
        }

        Ok(map)
    }

    async fn send_request(
        &self,
        api_base: &str,
        path: &str,
        method: reqwest::Method,
        body: &Bytes,
        content_type: Option<&str>,
        token: &str,
        initiator: Option<&str>,
        is_vision: bool,
        track_upstream: bool,
    ) -> Result<reqwest::Response, Error> {
        if track_upstream {
            let resolved_initiator = if initiator == Some("agent") {
                "agent"
            } else {
                "user"
            };
            crate::server::record_upstream(resolved_initiator, path);
        }

        let attempt = UpstreamAttempt::new(path);
        let mut headers = copilot_headers(
            &self.device_id,
            &self.machine_id,
            &self.session_id,
            initiator,
            is_vision,
        );
        headers.insert(
            "x-request-id",
            HeaderValue::from_str(&attempt.request_id).expect("UUID is a valid header value"),
        );
        let mut req = self
            .client
            .request(method, format!("{}{}", api_base, path))
            .bearer_auth(token)
            .headers(headers);

        if let Some(ct) = content_type {
            req = req.header("Content-Type", ct);
        }

        tracing::info!(
            diagnostic = "upstream_send_start",
            request_id = %attempt.request_id, route = attempt.route,
            request_bytes = body.len() as u64,
            "Copilot request body ready for forwarding"
        );
        match req.body(body.clone()).send().await {
            Ok(mut response) => {
                tracing::info!(
                    diagnostic = "upstream_headers",
                    request_id = %attempt.request_id, route = attempt.route,
                    elapsed_ms = attempt.started.elapsed().as_millis() as u64,
                    status = response.status().as_u16(), version = ?response.version(),
                    "Copilot response headers received"
                );
                response.extensions_mut().insert(attempt);
                Ok(response)
            }
            Err(error) => {
                tracing::warn!(
                    diagnostic = "upstream_send_error",
                    request_id = %attempt.request_id, route = attempt.route,
                    elapsed_ms = attempt.started.elapsed().as_millis() as u64,
                    failure = ?TransportFailure::classify(&error),
                    "Copilot request failed before response headers"
                );
                Err(error.into())
            }
        }
    }
}

fn cached_reasoning_efforts_from_cache(
    cache: &ModelsCache,
    keys: &[String],
) -> Option<Option<Vec<String>>> {
    let elapsed = cache.fetched_at.elapsed();
    if elapsed > MODELS_CACHE_TTL {
        return None;
    }

    let cached = keys
        .iter()
        .find_map(|key| cache.reasoning_efforts_by_model.get(key).cloned());
    if cached.is_some() || elapsed <= MODELS_UNKNOWN_REFRESH_TTL {
        return Some(cached);
    }

    None
}

fn model_lookup_keys(model: &str) -> Vec<String> {
    let key = model.to_lowercase();
    let mut keys = vec![key.clone()];

    let dashed_key = key.replace('.', "-");
    if dashed_key != key {
        keys.push(dashed_key);
    }

    if let Some(dotted_key) = dot_last_numeric_dash(&key)
        && !keys.contains(&dotted_key)
    {
        keys.push(dotted_key);
    }

    keys
}

fn dot_last_numeric_dash(key: &str) -> Option<String> {
    let (prefix, last_segment) = key.rsplit_once('-')?;
    let (_, previous_segment) = prefix.rsplit_once('-')?;
    if !last_segment.chars().all(|c| c.is_ascii_digit())
        || !previous_segment.chars().all(|c| c.is_ascii_digit())
    {
        return None;
    }
    Some(format!("{prefix}.{last_segment}"))
}

fn copilot_headers(
    device_id: &str,
    machine_id: &str,
    session_id: &str,
    initiator: Option<&str>,
    is_vision: bool,
) -> HeaderMap {
    let mut h = HeaderMap::new();
    h.insert("editor-version", HeaderValue::from_static("vscode/1.114.0"));
    h.insert(
        "editor-plugin-version",
        HeaderValue::from_static("copilot-chat/0.26.7"),
    );
    h.insert(
        "user-agent",
        HeaderValue::from_static("GitHubCopilotChat/0.26.7"),
    );
    h.insert(
        "x-github-api-version",
        HeaderValue::from_static("2026-01-09"),
    );
    h.insert(
        "copilot-integration-id",
        HeaderValue::from_static("vscode-chat"),
    );
    h.insert(
        "openai-intent",
        HeaderValue::from_static("conversation-agent"),
    );
    if let Ok(val) = HeaderValue::from_str(device_id) {
        h.insert("editor-device-id", val);
    }

    // Per-request unique ID, matching the real extension behavior
    if let Ok(val) = HeaderValue::from_str(&uuid::Uuid::new_v4().to_string()) {
        h.insert("x-request-id", val);
    }

    // Session context headers sent by the real VSCode Copilot extension.
    // These are used by the API for rate-limit bucketing and telemetry.
    if let Ok(val) = HeaderValue::from_str(machine_id) {
        h.insert("vscode-machineid", val);
    }
    if let Ok(val) = HeaderValue::from_str(session_id) {
        h.insert("vscode-sessionid", val);
    }

    // X-Initiator: "user" consumes premium, "agent" does not
    h.insert(
        "X-Initiator",
        HeaderValue::from_static(if initiator == Some("agent") {
            "agent"
        } else {
            "user"
        }),
    );

    if is_vision {
        h.insert("Copilot-Vision-Request", HeaderValue::from_static("true"));
    }

    h
}

/// Forward upstream response to client
pub async fn forward_response(resp: reqwest::Response) -> Result<Response, Error> {
    forward_response_inner(resp, false).await
}

/// Copilot may re-encode the same opaque item ID on every Responses SSE event.
/// Normalize only this explicit compatibility surface, not generic passthrough.
pub async fn forward_responses_response(resp: reqwest::Response) -> Result<Response, Error> {
    forward_response_inner(resp, true).await
}

async fn forward_response_inner(
    resp: reqwest::Response,
    normalize_ids: bool,
) -> Result<Response, Error> {
    let status = resp.status();
    let headers = resp.headers().clone();
    let attempt = resp
        .extensions()
        .get::<UpstreamAttempt>()
        .cloned()
        .unwrap_or_else(|| UpstreamAttempt::new("other"));
    let mut diagnostic = BodyDiagnostic::new(attempt);

    let is_stream = headers
        .get("content-type")
        .and_then(|v| v.to_str().ok())
        .map(|v| v.contains("text/event-stream"))
        .unwrap_or(false);

    let mut builder = Response::builder().status(status);

    for (key, value) in headers.iter() {
        if !HOP_BY_HOP.contains(&key.as_str())
            && !(normalize_ids
                && is_stream
                && matches!(key.as_str(), "content-length" | "etag" | "content-md5"))
        {
            builder = builder.header(key, value);
        }
    }

    if is_stream && !headers.contains_key("cache-control") {
        builder = builder.header("Cache-Control", "no-cache");
    }

    let body = if is_stream && normalize_ids {
        let upstream = Box::pin(diagnostic_bytes_stream(resp, diagnostic));
        let codec = crate::responses_sse::ResponsesSse::default();
        let stream = futures::stream::try_unfold(
            (upstream, codec, false),
            |(mut upstream, mut codec, mut eof)| async move {
                loop {
                    if eof {
                        return Ok::<_, std::io::Error>(None);
                    }
                    let bytes = match upstream.next().await {
                        Some(Ok(chunk)) => codec.push(&chunk)?,
                        Some(Err(error)) => return Err(std::io::Error::other(error)),
                        None => {
                            eof = true;
                            codec.finish()?
                        }
                    };
                    if !bytes.is_empty() {
                        return Ok(Some((Bytes::from(bytes), (upstream, codec, eof))));
                    }
                }
            },
        );
        Body::from_stream(stream)
    } else if is_stream {
        let stream = diagnostic_bytes_stream(resp, diagnostic).map_err(std::io::Error::other);
        Body::from_stream(stream)
    } else {
        match resp.bytes().await {
            Ok(bytes) => {
                diagnostic.chunk(bytes.len());
                diagnostic.finish(None);
                Body::from(bytes)
            }
            Err(error) => {
                diagnostic.finish(Some(&error));
                return Err(error.into());
            }
        }
    };

    builder
        .body(body)
        .map_err(|e| Error::InvalidRequest(e.to_string()))
}

#[cfg(test)]
#[path = "proxy_timeout_tests.rs"]
mod timeout_tests;

#[cfg(test)]
mod tests {
    use super::*;

    async fn synthetic_response(wire: &'static [u8]) -> reqwest::Response {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await.unwrap();
            let mut request = Vec::new();
            while !request.ends_with(b"\r\n\r\n") {
                let mut byte = [0];
                socket.read_exact(&mut byte).await.unwrap();
                request.push(byte[0]);
                assert!(request.len() < 16384);
            }
            socket.write_all(wire).await.unwrap();
            socket.shutdown().await.unwrap();
        });
        reqwest::Client::builder()
            .no_proxy()
            .build()
            .unwrap()
            .get(format!("http://{address}/?token=DO_NOT_LOG_SECRET"))
            .send()
            .await
            .unwrap()
    }

    #[tokio::test]
    async fn diagnostics_preserve_raw_sse_and_headers() {
        let response = synthetic_response(b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\nX-Test: preserved\r\n\r\n11\r\ndata: synthetic\n\n\r\n0\r\n\r\n").await;
        let response = forward_response(response).await.unwrap();
        assert_eq!(response.headers()["x-test"], "preserved");
        assert!(!response.headers().contains_key("transfer-encoding"));
        let body = axum::body::to_bytes(response.into_body(), 1024)
            .await
            .unwrap();
        assert_eq!(&body[..], b"data: synthetic\n\n");
    }

    #[tokio::test]
    async fn diagnostics_preserve_truncated_stream_error_and_redact_url() {
        let response = synthetic_response(b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n11\r\ndata: synthetic\n\n\r\n").await;
        let stream = diagnostic_bytes_stream(
            response,
            BodyDiagnostic::new(UpstreamAttempt::new(
                "/chat/completions?secret=DO_NOT_LOG_SECRET",
            )),
        );
        let mut stream = Box::pin(stream);
        let mut bytes = Vec::new();
        let error = loop {
            match stream.next().await {
                Some(Ok(chunk)) => bytes.extend_from_slice(&chunk),
                Some(Err(error)) => break error,
                None => panic!("truncated chunked stream must not become clean EOF"),
            }
        };
        assert_eq!(bytes, b"data: synthetic\n\n");
        let classification = TransportFailure::classify(&error);
        let safe = format!("{classification:?}");
        assert!(!safe.contains("DO_NOT_LOG_SECRET"));
        assert!(!safe.contains("http://"));
        assert!(classification.body || classification.decode);
        assert!(stream.next().await.is_none());
        assert_eq!(
            UpstreamAttempt::new("/responses?token=secret").route,
            "/responses"
        );
        assert_eq!(UpstreamAttempt::new("/private/secret").route, "other");
    }

    #[tokio::test]
    async fn diagnostics_preserve_buffered_response_and_error() {
        let response = synthetic_response(
            b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}",
        )
        .await;
        let response = forward_response(response).await.unwrap();
        let bytes = axum::body::to_bytes(response.into_body(), 1024)
            .await
            .unwrap();
        assert_eq!(&bytes[..], b"{}");
        let response = synthetic_response(
            b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 20\r\n\r\n{}",
        )
        .await;
        assert!(forward_response(response).await.is_err());
    }

    #[test]
    fn model_lookup_keys_include_dot_and_dash_spellings() {
        assert_eq!(
            model_lookup_keys("claude-opus-4.7"),
            vec!["claude-opus-4.7".to_string(), "claude-opus-4-7".to_string(),]
        );
        assert_eq!(
            model_lookup_keys("claude-opus-4-7"),
            vec!["claude-opus-4-7".to_string(), "claude-opus-4.7".to_string(),]
        );
    }

    #[test]
    fn fresh_unknown_model_cache_miss_is_cached_briefly() {
        let cache = ModelsCache {
            fetched_at: Instant::now(),
            reasoning_efforts_by_model: HashMap::new(),
        };
        let keys = vec!["brand-new-model".to_string()];

        assert_eq!(
            cached_reasoning_efforts_from_cache(&cache, &keys),
            Some(None)
        );
    }

    #[test]
    fn older_unknown_model_cache_miss_triggers_refresh() {
        let cache = ModelsCache {
            fetched_at: Instant::now() - MODELS_UNKNOWN_REFRESH_TTL - Duration::from_secs(1),
            reasoning_efforts_by_model: HashMap::new(),
        };
        let keys = vec!["brand-new-model".to_string()];

        assert_eq!(cached_reasoning_efforts_from_cache(&cache, &keys), None);
    }
}
