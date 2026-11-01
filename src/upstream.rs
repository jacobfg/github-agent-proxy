//! Forwarding to the real GitHub upstreams.
//!
//! Invariants enforced here:
//! - the client-supplied Authorization (and Cookie, Proxy-*, X-Forwarded-*)
//!   never reach the upstream; the proxy's PAT is injected instead
//! - redirects are never followed, so the PAT cannot leak via a 30x Location
//! - bodies pass through untouched (reqwest is built without compression
//!   features, so nothing is transparently decoded)
//!
//! Methods take owned arguments (`Arc<Self>`, `String`, `Parts`) rather than
//! references: every future in the request path must capture no external
//! regions, or rustc's auto-trait generalization bug ("implementation of
//! `Send` is not general enough") fires under axum's Handler bound.

use std::sync::Arc;

use axum::body::Body;
use axum::response::Response;
use http::header::HeaderName;
use http::request::Parts;

use crate::error::ProxyError;

const HOP_BY_HOP: [&str; 8] = [
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
];

/// Never forwarded, on top of hop-by-hop.
const STRIP_REQUEST: [&str; 8] = [
    "host",          // reqwest sets it from the upstream URL
    "authorization", // replaced with the proxy-held PAT
    "cookie",
    "content-length", // recomputed by reqwest for buffered bodies
    "x-forwarded-for",
    "x-forwarded-host",
    "x-forwarded-proto",
    "forwarded",
];

const STRIP_RESPONSE_EXTRA: [&str; 1] = ["content-length"]; // we stream chunked

/// How the proxy-held token is presented upstream.
enum AuthKind {
    /// `Authorization: Bearer <PAT>` — the REST/GraphQL API.
    Bearer,
    /// Basic `x-access-token:<PAT>` — required by git smart-HTTP endpoints.
    GitBasic,
}

fn is_stripped_request(name: &HeaderName) -> bool {
    let n = name.as_str();
    HOP_BY_HOP.contains(&n) || STRIP_REQUEST.contains(&n)
}

fn is_stripped_response(name: &HeaderName) -> bool {
    let n = name.as_str();
    HOP_BY_HOP.contains(&n) || STRIP_RESPONSE_EXTRA.contains(&n)
}

pub struct Upstream {
    client: reqwest::Client,
    pub api_base: String,
    pub git_base: String,
    token: String,
}

impl Upstream {
    pub fn new(api_base: String, git_base: String, token: String) -> anyhow::Result<Self> {
        let client = reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .user_agent(concat!("github-agent-proxy/", env!("CARGO_PKG_VERSION")))
            .build()?;
        Ok(Self {
            client,
            api_base: api_base.trim_end_matches('/').to_string(),
            git_base: git_base.trim_end_matches('/').to_string(),
            token,
        })
    }

    pub fn client(&self) -> &reqwest::Client {
        &self.client
    }

    pub async fn forward_api(
        self: Arc<Self>,
        path_and_query: String,
        parts: Parts,
        body: reqwest::Body,
    ) -> Result<Response<Body>, ProxyError> {
        let url = format!("{}{path_and_query}", self.api_base);
        self.forward(url, parts, body, AuthKind::Bearer).await
    }

    pub async fn forward_git(
        self: Arc<Self>,
        path_and_query: String,
        parts: Parts,
        body: reqwest::Body,
    ) -> Result<Response<Body>, ProxyError> {
        let url = format!("{}{path_and_query}", self.git_base);
        self.forward(url, parts, body, AuthKind::GitBasic).await
    }

    async fn forward(
        self: Arc<Self>,
        url: String,
        parts: Parts,
        body: reqwest::Body,
        auth: AuthKind,
    ) -> Result<Response<Body>, ProxyError> {
        let mut headers = reqwest::header::HeaderMap::new();
        for (name, value) in &parts.headers {
            if !is_stripped_request(name) {
                headers.append(name.clone(), value.clone());
            }
        }

        let mut req = self
            .client
            .request(parts.method.clone(), &url)
            .headers(headers)
            .header("via", "1.1 github-agent-proxy")
            .body(body);
        // The API accepts Bearer; git smart-HTTP endpoints only honor Basic
        // with x-access-token as the username (Bearer gets a 401 there).
        req = match auth {
            AuthKind::Bearer => req.bearer_auth(&self.token),
            AuthKind::GitBasic => req.basic_auth("x-access-token", Some(&self.token)),
        };
        let resp = req
            .send()
            .await
            .map_err(|e| ProxyError::upstream(format!("{e}")))?;

        let status = resp.status();
        let resp_headers = resp.headers().clone();
        let mut out = Response::new(Body::from_stream(resp.bytes_stream()));
        *out.status_mut() = status;
        for (name, value) in &resp_headers {
            if !is_stripped_response(name) {
                out.headers_mut().append(name.clone(), value.clone());
            }
        }
        Ok(out)
    }
}
