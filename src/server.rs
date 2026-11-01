use std::sync::Arc;
use std::time::Duration;

use crate::config::ServeConfig;
use crate::owner::{self, OwnerChecker};
use crate::policy::graphql::GraphQlPolicy;
use crate::proxy::{self, AppState};
use crate::upstream::Upstream;

pub async fn run(config: ServeConfig) -> anyhow::Result<()> {
    if config.allowed_orgs.is_empty() {
        anyhow::bail!("--allowed-orgs (GAP_ALLOWED_ORGS) is required and must not be empty");
    }
    let token = config.load_token()?;
    let upstream = Upstream::new(
        config.upstream_api.clone(),
        config.upstream_git.clone(),
        token.clone(),
    )?;

    // Fail closed: without an identity, owner-only rules cannot be enforced.
    let identity = match &config.owner_login {
        Some(login) => {
            tracing::warn!(%login, "using asserted owner login (GAP_OWNER_LOGIN) — owner-only semantics are only as good as this value");
            login.clone()
        }
        None => {
            let login =
                owner::resolve_identity(upstream.client(), &upstream.api_base, &token).await?;
            tracing::info!(%login, "resolved token owner");
            login
        }
    };

    let ttl = Duration::from_secs(config.owner_cache_ttl_secs);
    let owner = Arc::new(OwnerChecker::new(
        upstream.client().clone(),
        upstream.api_base.clone(),
        token.clone(),
        identity,
        ttl,
    ));
    let graphql = Arc::new(GraphQlPolicy::new(
        upstream.client().clone(),
        proxy::graphql_upstream_url(&config),
        token,
        ttl,
    ));
    let upstream = Arc::new(upstream);

    tracing::info!(
        orgs = ?config.allowed_orgs,
        prefix = %config.branch_prefix,
        "policy loaded"
    );

    let state = Arc::new(AppState {
        config: config.clone(),
        upstream,
        owner,
        graphql,
    });
    let app = proxy::router(state);

    match config.tls_config() {
        Some((cert, key)) => {
            let tls = axum_server::tls_rustls::RustlsConfig::from_pem_file(cert, key).await?;
            let handle = axum_server::Handle::new();
            let shutdown = handle.clone();
            tokio::spawn(async move {
                shutdown_signal().await;
                shutdown.graceful_shutdown(Some(Duration::from_secs(10)));
            });
            tracing::info!(addr = %config.listen, "listening (https)");
            axum_server::bind_rustls(config.listen, tls)
                .handle(handle)
                .serve(app.into_make_service())
                .await?;
        }
        None => {
            tracing::warn!(addr = %config.listen, "no TLS cert configured — serving plain HTTP (ok for git; gh requires TLS)");
            let listener = tokio::net::TcpListener::bind(config.listen).await?;
            axum::serve(listener, app)
                .with_graceful_shutdown(shutdown_signal())
                .await?;
        }
    }
    Ok(())
}

async fn shutdown_signal() {
    let ctrl_c = async {
        tokio::signal::ctrl_c().await.ok();
    };
    #[cfg(unix)]
    let terminate = async {
        if let Ok(mut sig) =
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
        {
            sig.recv().await;
        }
    };
    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();
    tokio::select! {
        _ = ctrl_c => {},
        _ = terminate => {},
    }
    tracing::info!("shutdown signal received");
}
