use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use clap::Args;

/// Liveness probe. Local only; never touches the upstream.
pub async fn healthz() -> Response {
    (StatusCode::OK, "ok\n").into_response()
}

#[derive(Args, Clone, Debug)]
pub struct HealthcheckArgs {
    /// Base URL of the proxy to probe. Defaults to https://$GAP_LISTEN/ with
    /// an http:// fallback, matching how the server binds.
    #[arg(long, env = "GAP_HEALTHCHECK_URL")]
    pub url: Option<String>,

    /// Skip TLS certificate verification (the proxy uses a local mkcert cert).
    #[arg(long, default_value_t = false)]
    pub insecure: bool,
}

fn listen_addr() -> String {
    let raw = std::env::var("GAP_LISTEN").unwrap_or_else(|_| "127.0.0.1:8443".to_string());
    // A wildcard listen address is not a connect address (and 0.0.0.0 is not
    // in any sane cert's SANs): probe loopback instead.
    raw.replace("0.0.0.0:", "127.0.0.1:")
        .replace("[::]:", "127.0.0.1:")
}

/// Returns a process exit code: 0 healthy, 1 otherwise.
pub async fn run_healthcheck(args: HealthcheckArgs) -> i32 {
    // The built-in loopback probe is a liveness ping carrying no data; skip
    // verification there (the proxy serves a local mkcert/self-signed cert
    // that the rustls webpki-roots client has no reason to trust). Explicit
    // --url targets keep verification unless --insecure is passed.
    let insecure = args.insecure || args.url.is_none();
    let client = match reqwest::Client::builder()
        .danger_accept_invalid_certs(insecure)
        .redirect(reqwest::redirect::Policy::none())
        .build()
    {
        Ok(c) => c,
        Err(e) => {
            eprintln!("healthcheck: cannot build client: {e}");
            return 1;
        }
    };

    let urls: Vec<String> = match &args.url {
        Some(base) => vec![format!("{}/healthz", base.trim_end_matches('/'))],
        None => {
            let addr = listen_addr();
            vec![
                format!("https://{addr}/healthz"),
                format!("http://{addr}/healthz"),
            ]
        }
    };

    for url in &urls {
        match client.get(url).send().await {
            Ok(resp) if resp.status().is_success() => {
                println!("healthy: {url}");
                return 0;
            }
            Ok(resp) => {
                eprintln!("healthcheck: {url} returned {}", resp.status());
            }
            Err(e) => {
                eprintln!("healthcheck: {url}: {e}");
            }
        }
    }
    1
}
