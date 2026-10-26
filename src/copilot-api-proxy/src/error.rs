//! Error types with OpenAI-compatible response formatting.

use axum::{
    Json,
    http::StatusCode,
    response::{IntoResponse, Response},
};
use thiserror::Error;

#[derive(Error, Debug)]
pub enum Error {
    #[error("Authentication failed: {0}")]
    Auth(String),
    #[error("Configuration error: {0}")]
    Config(String),
    #[error("Upstream request failed: {0}")]
    Upstream(#[from] reqwest::Error),
    #[error("Invalid request: {0}")]
    InvalidRequest(String),
    #[error("IO error: {0}")]
    Io(#[from] std::io::Error),
}

impl IntoResponse for Error {
    fn into_response(self) -> Response {
        let (status, error_type) = match &self {
            Error::Auth(_) => (StatusCode::UNAUTHORIZED, "authentication_error"),
            Error::Config(_) => (StatusCode::INTERNAL_SERVER_ERROR, "config_error"),
            Error::Upstream(_) => (StatusCode::BAD_GATEWAY, "upstream_error"),
            Error::InvalidRequest(_) => (StatusCode::BAD_REQUEST, "invalid_request_error"),
            Error::Io(_) => (StatusCode::INTERNAL_SERVER_ERROR, "io_error"),
        };

        let body = Json(serde_json::json!({
            "error": {
                "message": self.to_string(),
                "type": error_type,
                "param": null,
                "code": null
            }
        }));
        (status, body).into_response()
    }
}
