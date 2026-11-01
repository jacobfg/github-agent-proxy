use std::net::SocketAddr;
use std::path::PathBuf;

use clap::Args;

/// Configuration for the proxy server. Every option can be set via a `GAP_*`
/// environment variable; flags win over env.
#[derive(Args, Clone, Debug)]
pub struct ServeConfig {
    /// GitHub organisations (owner logins) that writes are allowed to target.
    /// Comma-separated, case-insensitive. Reads are never restricted.
    #[arg(long, env = "GAP_ALLOWED_ORGS", value_delimiter = ',')]
    pub allowed_orgs: Vec<String>,

    /// Branch prefix agents may create/update/delete, e.g. "agent/".
    #[arg(long, env = "GAP_BRANCH_PREFIX", default_value = "agent/")]
    pub branch_prefix: String,

    /// Fine-grained GitHub PAT injected into upstream requests. Prefer
    /// --token-file so the secret never appears in process listings.
    #[arg(
        long,
        env = "GAP_GITHUB_TOKEN",
        conflicts_with = "token_file",
        hide_env_values = true
    )]
    pub token: Option<String>,

    /// Path to a file containing the GitHub PAT.
    #[arg(long, env = "GAP_GITHUB_TOKEN_FILE")]
    pub token_file: Option<PathBuf>,

    /// Listen address. Loopback only — this port grants the proxy's write
    /// powers to anything that can reach it.
    #[arg(long, env = "GAP_LISTEN", default_value = "127.0.0.1:8443")]
    pub listen: SocketAddr,

    /// TLS certificate (PEM). Required for gh, which always speaks HTTPS.
    /// Generate with: mkcert localhost 127.0.0.1 ::1
    #[arg(long, env = "GAP_TLS_CERT", requires = "tls_key")]
    pub tls_cert: Option<PathBuf>,

    /// TLS private key (PEM).
    #[arg(long, env = "GAP_TLS_KEY", requires = "tls_cert")]
    pub tls_key: Option<PathBuf>,

    /// Upstream base for the REST/GraphQL API.
    #[arg(
        long,
        env = "GAP_UPSTREAM_API",
        default_value = "https://api.github.com"
    )]
    pub upstream_api: String,

    /// Upstream base for git smart-HTTP (and any other non-API traffic).
    #[arg(long, env = "GAP_UPSTREAM_GIT", default_value = "https://github.com")]
    pub upstream_git: String,

    /// GitHub Enterprise mode: preserve the /api/v3 path prefix when
    /// forwarding to the API upstream instead of stripping it.
    #[arg(long, env = "GAP_GHES", default_value_t = false)]
    pub ghes: bool,

    /// Asserted login of the PAT owner. Skips the startup GET /user lookup;
    /// a wrong value breaks owner-only semantics. Intended for tests and
    /// offline development.
    #[arg(long, env = "GAP_OWNER_LOGIN")]
    pub owner_login: Option<String>,

    /// Maximum bytes buffered from a git push body for ref inspection.
    #[arg(long, env = "GAP_MAX_PUSH_BODY_BYTES", default_value_t = 256 * 1024 * 1024)]
    pub max_push_body_bytes: usize,

    /// Maximum bytes buffered from any other request body that needs
    /// inspection (contents API, PR creation, GraphQL, LFS).
    #[arg(long, env = "GAP_MAX_BODY_BYTES", default_value_t = 16 * 1024 * 1024)]
    pub max_body_bytes: usize,

    /// TTL for cached author/repository lookups used by owner-only rules.
    #[arg(long, env = "GAP_OWNER_CACHE_TTL_SECS", default_value_t = 600)]
    pub owner_cache_ttl_secs: u64,
}

impl ServeConfig {
    /// Normalize derived forms: lowercase orgs, branch prefix ends in '/'.
    pub fn normalized(mut self) -> Self {
        self.allowed_orgs = self.allowed_orgs.iter().map(|o| o.to_lowercase()).collect();
        if !self.branch_prefix.is_empty() && !self.branch_prefix.ends_with('/') {
            self.branch_prefix.push('/');
        }
        self
    }

    /// Full git ref prefix, e.g. "refs/heads/agent/".
    pub fn branch_ref_prefix(&self) -> String {
        format!("refs/heads/{}", self.branch_prefix)
    }

    pub fn org_allowed(&self, org: &str) -> bool {
        self.allowed_orgs.iter().any(|o| o == &org.to_lowercase())
    }

    pub fn branch_allowed(&self, branch: &str) -> bool {
        branch.starts_with(&self.branch_prefix)
    }

    pub fn ref_allowed(&self, git_ref: &str) -> bool {
        git_ref.starts_with(&self.branch_ref_prefix())
    }

    pub fn tls_config(&self) -> Option<(&PathBuf, &PathBuf)> {
        match (&self.tls_cert, &self.tls_key) {
            (Some(c), Some(k)) => Some((c, k)),
            _ => None,
        }
    }

    /// Load the PAT from --token or --token-file.
    pub fn load_token(&self) -> anyhow::Result<String> {
        if let Some(token) = &self.token {
            return Ok(token.clone());
        }
        let path = self
            .token_file
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("a GitHub token is required: set --token (GAP_GITHUB_TOKEN) or --token-file (GAP_GITHUB_TOKEN_FILE)"))?;
        let raw = std::fs::read_to_string(path)
            .map_err(|e| anyhow::anyhow!("reading token file {}: {e}", path.display()))?;
        let token = raw.trim().to_string();
        if token.is_empty() {
            anyhow::bail!("token file {} is empty", path.display());
        }
        Ok(token)
    }
}
