use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Json;

/// Errors that terminate a proxied request before (or instead of) reaching
/// the upstream. Rendered as JSON for API clients; git push rejections are
/// handled separately via synthetic receive-pack results (see
/// `policy::git_push`).
#[derive(Debug, thiserror::Error)]
pub enum ProxyError {
    /// The request matched a deny rule, or failed an allow rule's checks.
    #[error("policy denied ({rule}): {message}")]
    PolicyDenied { rule: &'static str, message: String },

    /// The request was malformed (bad JSON body, unparseable pkt-line, ...).
    #[error("bad request: {0}")]
    BadRequest(String),

    /// A buffered body exceeded the configured cap.
    #[error("payload too large (limit {limit} bytes)")]
    TooLarge { limit: usize },

    /// The upstream could not be reached or returned something unusable.
    #[error("upstream error: {0}")]
    Upstream(String),
}

impl ProxyError {
    pub fn policy_denied(rule: &'static str, message: impl Into<String>) -> Self {
        Self::PolicyDenied {
            rule,
            message: message.into(),
        }
    }

    pub fn bad_request(message: impl Into<String>) -> Self {
        Self::BadRequest(message.into())
    }

    pub fn upstream(message: impl Into<String>) -> Self {
        Self::Upstream(message.into())
    }

    fn status(&self) -> StatusCode {
        match self {
            Self::PolicyDenied { .. } => StatusCode::FORBIDDEN,
            Self::BadRequest(_) => StatusCode::BAD_REQUEST,
            Self::TooLarge { .. } => StatusCode::PAYLOAD_TOO_LARGE,
            Self::Upstream(_) => StatusCode::BAD_GATEWAY,
        }
    }
}

impl IntoResponse for ProxyError {
    fn into_response(self) -> Response {
        let status = self.status();
        let (kind, rule, message) = match &self {
            Self::PolicyDenied { rule, message } => ("policy_denied", Some(*rule), message.clone()),
            Self::BadRequest(m) => ("bad_request", None, m.clone()),
            Self::TooLarge { .. } => ("payload_too_large", None, self.to_string()),
            Self::Upstream(m) => ("upstream_error", None, m.clone()),
        };
        let mut body = serde_json::json!({
            "error": kind,
            "message": message,
        });
        if let Some(rule) = rule {
            body["rule"] = serde_json::Value::String(rule.to_string());
        }
        (status, Json(body)).into_response()
    }
}
