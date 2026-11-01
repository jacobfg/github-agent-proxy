use clap::{Parser, Subcommand};
use github_agent_proxy::config::ServeConfig;
use github_agent_proxy::health::HealthcheckArgs;
use tracing_subscriber::EnvFilter;

/// Reverse proxy enforcing fine-grained write policy for coding agents using
/// the gh and git CLIs. The proxy holds the real GitHub credentials; clients
/// need none.
///
/// With no subcommand, runs the proxy (`serve`).
#[derive(Parser)]
#[command(name = "github-agent-proxy", version, about)]
#[command(args_conflicts_with_subcommands = true)]
struct Cli {
    #[command(flatten)]
    serve: ServeConfig,

    #[command(subcommand)]
    command: Option<Commands>,
}

#[derive(Subcommand)]
enum Commands {
    /// Probe a running proxy's /healthz; exits non-zero unless healthy.
    Healthcheck(HealthcheckArgs),
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    // Both aws-lc-rs (rustls default) and ring (via rcgen, dev-only) end up
    // compiled in; rustls then refuses to auto-pick a process CryptoProvider.
    rustls::crypto::aws_lc_rs::default_provider()
        .install_default()
        .map_err(|_| anyhow::anyhow!("failed to install rustls CryptoProvider"))?;

    tracing_subscriber::fmt()
        .with_env_filter(
            EnvFilter::try_from_env("GAP_LOG")
                .or_else(|_| EnvFilter::try_from_default_env())
                .unwrap_or_else(|_| EnvFilter::new("info")),
        )
        .init();

    let cli = Cli::parse();
    match cli.command {
        Some(Commands::Healthcheck(args)) => {
            let code = github_agent_proxy::health::run_healthcheck(args).await;
            std::process::exit(code);
        }
        None => github_agent_proxy::server::run(cli.serve.normalized()).await,
    }
}
