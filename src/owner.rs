//! Owner-only enforcement: resolves the PAT identity at startup and answers
//! "was this resource authored by the token owner?" with a cached lookup.
//!
//! Methods take owned arguments (`Arc<Self>`, `String`) so futures in the
//! request path capture no external regions — see `upstream.rs` for why.

use std::sync::Arc;
use std::time::Duration;

use moka::future::Cache;

use crate::error::ProxyError;

/// Answers author-ownership questions about GitHub resources, with a TTL
/// cache. Authors are immutable on GitHub, so the TTL is only a safety valve
/// for upstream anomalies.
pub struct OwnerChecker {
    client: reqwest::Client,
    api_base: String,
    token: String,
    identity: String,
    /// Key: upstream GET path (e.g. "/repos/o/r/issues/42"). Value: author login.
    cache: Cache<String, String>,
}

impl OwnerChecker {
    pub fn new(
        client: reqwest::Client,
        api_base: String,
        token: String,
        identity: String,
        ttl: Duration,
    ) -> Self {
        let cache = Cache::builder()
            .time_to_live(ttl)
            .max_capacity(10_000)
            .build();
        Self {
            client,
            api_base,
            token,
            identity,
            cache,
        }
    }

    pub fn identity(&self) -> &str {
        &self.identity
    }

    /// Fetch `GET {api_base}{path}` and extract `user.login`.
    async fn fetch_author(self: Arc<Self>, path: String) -> Result<String, ProxyError> {
        let resp = self
            .client
            .get(format!("{}{path}", self.api_base))
            .bearer_auth(&self.token)
            .header("accept", "application/vnd.github+json")
            .send()
            .await
            .map_err(|e| ProxyError::upstream(format!("owner check request failed: {e}")))?;
        if !resp.status().is_success() {
            // Fail closed. Deliberately indistinguishable from a 404 so the
            // proxy is not an existence oracle for resources the PAT cannot
            // see.
            return Err(ProxyError::policy_denied(
                "owner_check",
                "could not verify authorship of the target resource",
            ));
        }
        let body: serde_json::Value = resp
            .json()
            .await
            .map_err(|e| ProxyError::upstream(format!("owner check response not JSON: {e}")))?;
        body.pointer("/user/login")
            .and_then(|v| v.as_str())
            .map(str::to_string)
            .ok_or_else(|| ProxyError::upstream("owner check response missing user.login"))
    }

    /// Author login for the resource at `path`, cached with singleflight.
    pub async fn author_of(self: Arc<Self>, path: String) -> Result<String, ProxyError> {
        let result = self
            .cache
            .try_get_with(path.clone(), self.clone().fetch_author(path))
            .await;
        match result {
            Ok(author) => {
                tracing::debug!(author, "owner lookup");
                Ok(author)
            }
            // try_get_with wraps errors in Arc; unwrap our own error type.
            Err(e) => Err(Arc::try_unwrap(e)
                .unwrap_or_else(|e| ProxyError::policy_denied("owner_check", e.to_string()))),
        }
    }

    /// True when the resource at `path` was authored by the PAT owner.
    pub async fn is_owner(self: Arc<Self>, path: String) -> Result<bool, ProxyError> {
        let identity = self.identity.clone();
        let author = self.author_of(path).await?;
        Ok(author.eq_ignore_ascii_case(&identity))
    }
}

/// Resolve the PAT owner's login via `GET /user`. Called once at startup;
/// failure aborts startup (owner-only rules cannot be enforced without an
/// identity, and failing closed beats running unenforced). Not on the
/// per-request path, so plain references are fine here.
pub async fn resolve_identity(
    client: &reqwest::Client,
    api_base: &str,
    token: &str,
) -> anyhow::Result<String> {
    let resp = client
        .get(format!("{api_base}/user"))
        .bearer_auth(token)
        .header("accept", "application/vnd.github+json")
        .send()
        .await
        .map_err(|e| anyhow::anyhow!("GET /user failed: {e}"))?;
    if !resp.status().is_success() {
        anyhow::bail!("GET /user returned {} — is the token valid?", resp.status());
    }
    let body: serde_json::Value = resp.json().await?;
    body.pointer("/login")
        .and_then(|v| v.as_str())
        .map(str::to_string)
        .ok_or_else(|| anyhow::anyhow!("GET /user response missing login"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use wiremock::matchers::{method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    fn test_client() -> reqwest::Client {
        reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .unwrap()
    }

    #[tokio::test]
    async fn resolve_identity_reads_login() {
        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/user"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "login": "agent-bot"
            })))
            .mount(&server)
            .await;
        let login = resolve_identity(&test_client(), &server.uri(), "tok")
            .await
            .unwrap();
        assert_eq!(login, "agent-bot");
    }

    #[tokio::test]
    async fn owner_check_caches_and_matches_case_insensitively() {
        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/repos/o/r/issues/42"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "user": {"login": "Agent-Bot"}
            })))
            .expect(1) // second call must be a cache hit
            .mount(&server)
            .await;

        let checker = Arc::new(OwnerChecker::new(
            test_client(),
            server.uri(),
            "tok".into(),
            "agent-bot".into(),
            Duration::from_secs(60),
        ));
        assert!(checker
            .clone()
            .is_owner("/repos/o/r/issues/42".into())
            .await
            .unwrap());
        assert!(checker
            .is_owner("/repos/o/r/issues/42".into())
            .await
            .unwrap());
    }

    #[tokio::test]
    async fn owner_check_fails_closed_on_upstream_error() {
        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/repos/o/r/issues/1"))
            .respond_with(ResponseTemplate::new(404))
            .mount(&server)
            .await;

        let checker = Arc::new(OwnerChecker::new(
            test_client(),
            server.uri(),
            "tok".into(),
            "agent-bot".into(),
            Duration::from_secs(60),
        ));
        let err = checker
            .is_owner("/repos/o/r/issues/1".into())
            .await
            .unwrap_err();
        assert!(matches!(err, ProxyError::PolicyDenied { .. }));
    }
}
