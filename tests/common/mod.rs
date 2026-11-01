//! Shared test harness: a real proxy on loopback in front of mock upstreams.
//! The git upstream is a tiny CGI bridge over the real `git http-backend`
//! binary backed by a bare repo in a tempdir, so push/fetch tests exercise
//! the genuine smart-HTTP protocol.

#![allow(dead_code)]

use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use github_agent_proxy::config::ServeConfig;
use github_agent_proxy::owner::OwnerChecker;
use github_agent_proxy::policy::graphql::GraphQlPolicy;
use github_agent_proxy::proxy::{self, AppState};
use github_agent_proxy::upstream::Upstream;

pub const IDENTITY: &str = "agent-bot";

pub struct TestProxy {
    pub base: String, // http://127.0.0.1:PORT
    pub api: wiremock::MockServer,
    pub git_base: String,
}

fn test_config(api_base: &str, git_base: &str, listen: SocketAddr) -> ServeConfig {
    use clap::Parser as _;

    #[derive(clap::Parser)]
    struct TestCli {
        #[command(flatten)]
        serve: ServeConfig,
    }
    TestCli::parse_from([
        "test",
        "--allowed-orgs",
        "myorg,otherorg",
        "--token",
        "test-pat",
        "--owner-login",
        IDENTITY,
        "--listen",
        &listen.to_string(),
        "--upstream-api",
        api_base,
        "--upstream-git",
        git_base,
    ])
    .serve
    .normalized()
}

pub async fn start() -> TestProxy {
    let api = wiremock::MockServer::start().await;
    let uri = api.uri();
    start_with(api, &uri).await
}

/// Start a plain-HTTP proxy with the given git upstream (CGI bridge) and a
/// wiremock API upstream.
pub async fn start_with(api: wiremock::MockServer, git_base: &str) -> TestProxy {
    let _ = tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::new(
            std::env::var("GAP_TEST_LOG").unwrap_or_else(|_| "info".into()),
        ))
        .with_test_writer()
        .try_init();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();

    let config = test_config(&api.uri(), git_base, addr);
    let token = "test-pat".to_string();
    let up = Upstream::new(
        config.upstream_api.clone(),
        config.upstream_git.clone(),
        token.clone(),
    )
    .unwrap();
    let ttl = Duration::from_secs(60);
    let owner = OwnerChecker::new(
        up.client().clone(),
        up.api_base.clone(),
        token.clone(),
        IDENTITY.into(),
        ttl,
    );
    let graphql = GraphQlPolicy::new(
        up.client().clone(),
        proxy::graphql_upstream_url(&config),
        token,
        ttl,
    );
    let state = Arc::new(AppState {
        config,
        upstream: Arc::new(up),
        owner: Arc::new(owner),
        graphql: Arc::new(graphql),
    });
    let app = proxy::router(state);
    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });

    TestProxy {
        base: format!("http://{addr}"),
        api,
        git_base: git_base.to_string(),
    }
}

pub fn client() -> reqwest::Client {
    reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .unwrap()
}

pub fn git_available() -> bool {
    std::process::Command::new("git")
        .arg("--version")
        .output()
        .is_ok()
}

/// A bare repo (myorg/r.git, receive-pack enabled) served over CGI by the
/// real git http-backend binary.
pub struct GitUpstream {
    pub base: String,
    pub root: PathBuf,
    _tmp: tempfile::TempDir,
}

pub async fn start_git_upstream() -> GitUpstream {
    assert!(git_available(), "git binary required for e2e tests");
    let tmp = tempfile::tempdir().unwrap();
    let root = tmp.path().join("repos");
    let bare = root.join("myorg").join("r.git");
    std::fs::create_dir_all(&bare).unwrap();
    let bare_str = bare.to_str().unwrap().to_string();
    for args in [
        vec!["init", "--bare", "--initial-branch=main", &bare_str],
        vec!["-C", &bare_str, "config", "http.receivepack", "true"],
    ] {
        let out = std::process::Command::new("git")
            .args(&args)
            .output()
            .unwrap();
        assert!(out.status.success(), "git {args:?}: {:?}", out.stderr);
    }

    // `git http-backend` isn't dispatched as a subcommand everywhere
    // (Alpine); invoke the binary from git's exec-path directly.
    let exec_path = std::process::Command::new("git")
        .arg("--exec-path")
        .output()
        .unwrap();
    assert!(exec_path.status.success());
    let backend = PathBuf::from(
        String::from_utf8_lossy(&exec_path.stdout)
            .trim()
            .to_string(),
    )
    .join("git-http-backend");
    assert!(
        backend.exists(),
        "git-http-backend not found at {}",
        backend.display()
    );

    #[derive(Clone)]
    struct Cgi {
        root: Arc<PathBuf>,
        backend: Arc<PathBuf>,
    }

    fn plain_response(status: u16, body: String) -> axum::response::Response {
        let mut resp = axum::response::Response::new(axum::body::Body::from(body));
        *resp.status_mut() = axum::http::StatusCode::from_u16(status).unwrap();
        resp.headers_mut().insert(
            axum::http::header::CONTENT_TYPE,
            "text/plain".parse().unwrap(),
        );
        resp
    }

    async fn cgi_bridge(
        axum::extract::State(cgi): axum::extract::State<Cgi>,
        req: axum::extract::Request,
    ) -> axum::response::Response {
        use http_body_util::BodyExt;
        let (parts, body) = req.into_parts();
        let body = body.collect().await.unwrap().to_bytes();

        let mut cmd = std::process::Command::new(&*cgi.backend);
        cmd.env("GIT_PROJECT_ROOT", &*cgi.root)
            .env("GIT_HTTP_EXPORT_ALL", "1")
            .env("PATH_INFO", parts.uri.path())
            .env("REQUEST_METHOD", parts.method.as_str())
            .env("QUERY_STRING", parts.uri.query().unwrap_or(""))
            .env("CONTENT_LENGTH", body.len().to_string())
            .env(
                "CONTENT_TYPE",
                parts
                    .headers
                    .get("content-type")
                    .and_then(|v| v.to_str().ok())
                    .unwrap_or(""),
            )
            .env(
                "HTTP_CONTENT_ENCODING",
                parts
                    .headers
                    .get("content-encoding")
                    .and_then(|v| v.to_str().ok())
                    .unwrap_or(""),
            )
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped());
        let mut child = match cmd.spawn() {
            Ok(c) => c,
            Err(e) => {
                return plain_response(500, format!("git http-backend spawn failed: {e}"));
            }
        };
        if let Err(e) = child.stdin.as_mut().unwrap().write_all(&body) {
            return plain_response(500, format!("git http-backend stdin write failed: {e}"));
        }
        let out = child.wait_with_output().unwrap();
        if !out.status.success() {
            let stderr = String::from_utf8_lossy(&out.stderr);
            eprintln!("git http-backend exited {}: {}", out.status, stderr);
            return plain_response(500, format!("git http-backend failed: {stderr}"));
        }

        // CGI: headers, blank line, body
        let raw = out.stdout;
        let Some(split) = raw
            .windows(4)
            .position(|w| w == b"\r\n\r\n")
            .map(|i| (i, 4))
            .or_else(|| raw.windows(2).position(|w| w == b"\n\n").map(|i| (i, 2)))
        else {
            let head = String::from_utf8_lossy(&raw[..raw.len().min(500)]);
            let stderr = String::from_utf8_lossy(&out.stderr);
            eprintln!(
                "git http-backend: no CGI header separator; stdout={head:?} stderr={stderr:?}"
            );
            return plain_response(502, format!("bad CGI output: {head}; stderr: {stderr}"));
        };
        let (head, cgi_body) = raw.split_at(split.0);
        let cgi_body = &cgi_body[split.1..];

        let mut status = 200u16;
        let mut content_type = "application/octet-stream".to_string();
        for line in String::from_utf8_lossy(head).lines() {
            if let Some((name, value)) = line.split_once(':') {
                let name = name.trim().to_lowercase();
                let value = value.trim();
                if name == "status" {
                    status = value
                        .split_whitespace()
                        .next()
                        .and_then(|s| s.parse().ok())
                        .unwrap_or(200);
                } else if name == "content-type" {
                    content_type = value.to_string();
                }
            }
        }

        let mut resp = axum::response::Response::new(axum::body::Body::from(cgi_body.to_vec()));
        *resp.status_mut() = axum::http::StatusCode::from_u16(status).unwrap();
        resp.headers_mut().insert(
            axum::http::header::CONTENT_TYPE,
            content_type.parse().unwrap(),
        );
        resp
    }

    let app = axum::Router::new().fallback(cgi_bridge).with_state(Cgi {
        root: Arc::new(root.clone()),
        backend: Arc::new(backend),
    });
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });

    GitUpstream {
        base: format!("http://{addr}"),
        root,
        _tmp: tmp,
    }
}

/// Run git with the proxy rewrite applied, in `dir`.
pub fn git(dir: &Path, proxy_base: &str, args: &[&str]) -> (bool, String, String) {
    let rewrite = format!("url.{proxy_base}/.insteadOf=https://github.com/");
    let mut cmd = std::process::Command::new("git");
    cmd.arg("-c")
        .arg(&rewrite)
        .args(args)
        .current_dir(dir)
        .env("GIT_TERMINAL_PROMPT", "0");
    if std::env::var("GAP_TEST_TRACE").is_ok() {
        cmd.env("GIT_TRACE_PACKET", "1").env("GIT_TRACE", "1");
    }
    let out = cmd.output().unwrap();
    (
        out.status.success(),
        String::from_utf8_lossy(&out.stdout).into_owned(),
        String::from_utf8_lossy(&out.stderr).into_owned(),
    )
}

use std::io::Write as _;
