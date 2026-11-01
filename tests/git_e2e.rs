//! End-to-end with the real git binary: clone, push, force-push, and delete
//! through the proxy, against a real git http-backend upstream.

mod common;

use common::{git, git_available, start_git_upstream, start_with};

// Multi-thread runtime: the test drives the real git binary with blocking
// std::process calls, and the proxy + CGI upstream run as tasks on the same
// runtime — a single-threaded runtime would deadlock.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn git_push_policy_end_to_end() {
    if !git_available() {
        eprintln!("git binary not available; skipping e2e");
        return;
    }

    let upstream = start_git_upstream().await;
    let api = wiremock::MockServer::start().await;
    let proxy = start_with(api, &upstream.base).await;

    let work = tempfile::tempdir().unwrap();
    let dir = work.path();

    // Clone the (empty) repo through the proxy via insteadOf — the command
    // line mentions only github.com, exactly like an agent's would.
    let (ok, _stdout, stderr) = git(
        dir,
        &proxy.base,
        &["clone", "https://github.com/myorg/r.git", "work"],
    );
    assert!(ok, "clone failed: {stderr}");
    let repo = dir.join("work");

    // Make a commit.
    let (ok, _, stderr) = git(
        &repo,
        &proxy.base,
        &[
            "-c",
            "user.email=t@t",
            "-c",
            "user.name=t",
            "commit",
            "--allow-empty",
            "-m",
            "init",
        ],
    );
    assert!(ok, "commit failed: {stderr}");

    // Push to a prefixed branch: allowed.
    let (ok, _o, stderr) = git(
        &repo,
        &proxy.base,
        &["push", "origin", "HEAD:refs/heads/agent/feature"],
    );
    assert!(ok, "prefixed push should succeed: {stderr}");

    // Push to main: remote-rejected with a readable policy message.
    let (ok, _o, stderr) = git(
        &repo,
        &proxy.base,
        &["push", "origin", "HEAD:refs/heads/main"],
    );
    assert!(!ok, "push to main must fail");
    assert!(
        stderr.contains("remote rejected") && stderr.contains("policy:"),
        "expected policy rejection, got: {stderr}"
    );

    // Force-push within the prefix: allowed.
    let (ok, _, stderr) = git(
        &repo,
        &proxy.base,
        &["push", "--force", "origin", "HEAD:refs/heads/agent/feature"],
    );
    assert!(ok, "force push within prefix should succeed: {stderr}");

    // Delete within the prefix: allowed.
    let (ok, _o, stderr) = git(
        &repo,
        &proxy.base,
        &["push", "origin", ":refs/heads/agent/feature"],
    );
    assert!(ok, "delete within prefix should succeed: {stderr}");

    // Delete outside the prefix: rejected.
    let (ok, _o, stderr) = git(&repo, &proxy.base, &["push", "origin", ":refs/heads/main"]);
    assert!(!ok, "delete of main must fail");
    assert!(stderr.contains("remote rejected"), "got: {stderr}");

    // Fetch still works after all this (reads unfiltered).
    let (ok, _o, stderr) = git(&repo, &proxy.base, &["fetch", "origin"]);
    assert!(ok, "fetch failed: {stderr}");
}

// A server-side rejection from the real backend, forwarded through the proxy,
// must render as a clean [remote rejected] (regression coverage for the
// synthetic-rejection path: framing must behave identically).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn forwarded_upstream_rejection() {
    if !common::git_available() {
        return;
    }
    let upstream = common::start_git_upstream().await;
    let api = wiremock::MockServer::start().await;
    let proxy = common::start_with(api, &upstream.base).await;

    let work = tempfile::tempdir().unwrap();
    let dir = work.path();
    let (ok, _, stderr) = git(
        dir,
        &proxy.base,
        &["clone", "https://github.com/myorg/r.git", "work"],
    );
    assert!(ok, "clone failed: {stderr}");
    let repo = dir.join("work");
    let (ok, _, stderr) = git(
        &repo,
        &proxy.base,
        &[
            "-c",
            "user.email=t@t",
            "-c",
            "user.name=t",
            "commit",
            "--allow-empty",
            "-m",
            "one",
        ],
    );
    assert!(ok, "commit: {stderr}");
    let (ok, _, stderr) = git(
        &repo,
        &proxy.base,
        &["push", "origin", "HEAD:refs/heads/agent/feature"],
    );
    assert!(ok, "first push: {stderr}");
    // Force a *server-side* rejection the client cannot foresee:
    // denyNonFastForwards on the bare repo + a forced non-ff push.
    let bare = upstream.root.join("myorg/r.git");
    let out = std::process::Command::new("git")
        .args([
            "-C",
            bare.to_str().unwrap(),
            "config",
            "receive.denyNonFastForwards",
            "true",
        ])
        .output()
        .unwrap();
    assert!(out.status.success());
    // diverge: amend so the next push is non-fast-forward
    let (ok, _, stderr) = git(
        &repo,
        &proxy.base,
        &[
            "-c",
            "user.email=t@t",
            "-c",
            "user.name=t",
            "commit",
            "--amend",
            "--allow-empty",
            "-m",
            "two",
        ],
    );
    assert!(ok, "amend: {stderr}");
    let (ok, _o, stderr) = git(
        &repo,
        &proxy.base,
        &["push", "--force", "origin", "HEAD:refs/heads/agent/feature"],
    );
    assert!(!ok, "forced non-ff push must fail server-side");
    eprintln!("=== forwarded rejection stderr ===\n{stderr}\n=== end ===");
    assert!(
        stderr.contains("remote rejected"),
        "expected clean upstream rejection, got: {stderr}"
    );
}
