pub mod classify;
pub mod normalize;

use std::sync::Arc;
use std::time::Instant;

use axum::body::Body;
use axum::extract::State;
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::routing::get;
use axum::Router;
use http_body_util::BodyExt;
use tower_http::trace::TraceLayer;

use crate::config::ServeConfig;
use crate::error::ProxyError;
use crate::health;
use crate::owner::OwnerChecker;
use crate::policy::git_push::{self, PushDecision};
use crate::policy::graphql::GraphQlPolicy;
use crate::policy::rules;
use crate::upstream::Upstream;
use classify::{classify_api, classify_git, Classified, Rule};
use normalize::{normalize, Route};

pub struct AppState {
    pub config: ServeConfig,
    pub upstream: Arc<Upstream>,
    pub owner: Arc<OwnerChecker>,
    pub graphql: Arc<GraphQlPolicy>,
}

pub fn router(state: Arc<AppState>) -> Router {
    Router::new()
        .route("/healthz", get(health::healthz))
        .fallback(handle_request)
        .layer(TraceLayer::new_for_http())
        .with_state(state)
}

/// Upstream URL for GraphQL. On GHES the API base already ends in /api/v3
/// but GraphQL lives at /api/graphql, so swap the prefix.
pub fn graphql_upstream_url(config: &ServeConfig) -> String {
    let base = config.upstream_api.trim_end_matches('/');
    if config.ghes {
        if let Some(root) = base.strip_suffix("/api/v3") {
            return format!("{root}/api/graphql");
        }
    }
    format!("{base}/graphql")
}

async fn handle_request(
    State(state): State<Arc<AppState>>,
    req: axum::extract::Request,
) -> Response {
    let started = Instant::now();
    let method = req.method().clone();
    let path = req.uri().path().to_string();
    match dispatch(state, req).await {
        Ok(resp) => {
            tracing::info!(%method, %path, status = %resp.status(), elapsed_ms = started.elapsed().as_millis(), "proxied");
            resp
        }
        Err(e) => {
            let status = match &e {
                ProxyError::PolicyDenied { .. } => "denied",
                _ => "error",
            };
            tracing::info!(%method, %path, error = %e, outcome = status, elapsed_ms = started.elapsed().as_millis(), "rejected");
            e.into_response()
        }
    }
}

async fn buffer_body(body: Body, limit: usize) -> Result<bytes::Bytes, ProxyError> {
    let mut body = std::pin::pin!(body);
    let mut buf = bytes::BytesMut::new();
    while let Some(frame) = body.frame().await {
        let frame =
            frame.map_err(|e| ProxyError::bad_request(format!("reading request body: {e}")))?;
        if let Ok(data) = frame.into_data() {
            if buf.len() + data.len() > limit {
                return Err(ProxyError::TooLarge { limit });
            }
            buf.extend_from_slice(&data);
        }
    }
    Ok(buf.freeze())
}

fn stream_body(body: Body) -> reqwest::Body {
    reqwest::Body::wrap_stream(body.into_data_stream())
}

fn default_deny() -> ProxyError {
    ProxyError::policy_denied(
        "default_deny",
        "this write operation is not permitted by the proxy policy",
    )
}

// Everything in the call tree below takes owned arguments (Arc clones,
// Strings, Bytes, Parts). An async fn whose future captures reference
// *parameters* fails axum's Handler Send bound with rustc's
// "implementation of `Send` is not general enough" false negative; owned
// arguments keep every future in the tree region-free.
async fn dispatch(
    state: Arc<AppState>,
    req: axum::extract::Request,
) -> Result<Response, ProxyError> {
    let (parts, body) = req.into_parts();
    let pq = parts
        .uri
        .path_and_query()
        .map(|pq| pq.as_str().to_string())
        .unwrap_or_else(|| "/".to_string());

    match normalize(&pq, state.config.ghes) {
        Route::GraphQl => {
            let bytes = buffer_body(body, state.config.max_body_bytes).await?;
            state
                .graphql
                .clone()
                .evaluate(
                    bytes.clone(),
                    state.config.clone(),
                    state.owner.identity().to_string(),
                )
                .await?;
            state
                .upstream
                .clone()
                .forward_api("/graphql".to_string(), parts, reqwest::Body::from(bytes))
                .await
        }

        Route::Api(path) => {
            let path_only = path.split('?').next().unwrap_or("/");
            match classify_api(&parts.method, path_only) {
                Classified::Read => {
                    state
                        .upstream
                        .clone()
                        .forward_api(path, parts, stream_body(body))
                        .await
                }
                Classified::Denied => Err(default_deny()),
                Classified::RestWrite(rule) => {
                    let inspects_body = matches!(
                        rule,
                        Rule::CreatePullRequest { .. }
                            | Rule::GitRefsCreate { .. }
                            | Rule::Contents { .. }
                    );
                    if inspects_body {
                        let bytes = buffer_body(body, state.config.max_body_bytes).await?;
                        rules::evaluate(
                            rule,
                            bytes.clone(),
                            state.config.clone(),
                            state.owner.clone(),
                        )
                        .await?;
                        state
                            .upstream
                            .clone()
                            .forward_api(path, parts, reqwest::Body::from(bytes))
                            .await
                    } else {
                        rules::evaluate(
                            rule,
                            bytes::Bytes::new(),
                            state.config.clone(),
                            state.owner.clone(),
                        )
                        .await?;
                        state
                            .upstream
                            .clone()
                            .forward_api(path, parts, stream_body(body))
                            .await
                    }
                }
                // classify_api never produces these; fail closed anyway.
                _ => Err(default_deny()),
            }
        }

        Route::Git(git_pq) => match classify_git(&parts.method, &git_pq) {
            Classified::Read => {
                state
                    .upstream
                    .clone()
                    .forward_git(git_pq, parts, stream_body(body))
                    .await
            }
            Classified::Denied => Err(default_deny()),
            Classified::LfsBatch { org, .. } => {
                let bytes = buffer_body(body, state.config.max_body_bytes).await?;
                rules::evaluate_lfs(&org, &bytes, &state.config)?;
                state
                    .upstream
                    .clone()
                    .forward_git(git_pq, parts, reqwest::Body::from(bytes))
                    .await
            }
            Classified::GitReceivePack { org, .. } => {
                receive_pack(state, parts, body, git_pq, org).await
            }
            Classified::RestWrite(_) => Err(default_deny()),
        },
    }
}

async fn receive_pack(
    state: Arc<AppState>,
    parts: http::request::Parts,
    body: Body,
    git_pq: String,
    org: String,
) -> Result<Response, ProxyError> {
    if !state.config.org_allowed(&org) {
        return Err(ProxyError::policy_denied(
            "org_allowlist",
            format!("organisation `{org}` is not in GAP_ALLOWED_ORGS"),
        ));
    }
    if let Some(protocol) = parts
        .headers
        .get("git-protocol")
        .and_then(|v| v.to_str().ok())
    {
        if protocol.contains("version=2") {
            return Err(ProxyError::policy_denied(
                "git_protocol",
                "git protocol version 2 push is not supported; unset Git-Protocol or use protocol v0",
            ));
        }
    }
    let gzipped = parts
        .headers
        .get("content-encoding")
        .and_then(|v| v.to_str().ok())
        .is_some_and(|v| v.eq_ignore_ascii_case("gzip"));

    let bytes = buffer_body(body, state.config.max_push_body_bytes).await?;
    let decision = git_push::evaluate(&bytes, gzipped, &state.config)?;
    tracing::info!(
        body_len = bytes.len(),
        gzipped,
        decision = matches!(decision, PushDecision::Reject(_)),
        "receive-pack decision"
    );
    match decision {
        PushDecision::Forward => {
            state
                .upstream
                .clone()
                .forward_git(git_pq, parts, reqwest::Body::from(bytes))
                .await
        }
        PushDecision::Reject(report) => {
            let mut resp = Response::new(Body::from(report));
            *resp.status_mut() = StatusCode::OK;
            resp.headers_mut().insert(
                http::header::CONTENT_TYPE,
                "application/x-git-receive-pack-result".parse().unwrap(),
            );
            Ok(resp)
        }
    }
}
