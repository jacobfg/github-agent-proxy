//! Evaluation of REST write rules. Every rule passes through the org check
//! first; rules then add body-field and owner checks.
//!
//! `evaluate` takes owned arguments (`Rule`, `Bytes`, `ServeConfig`,
//! `Arc<OwnerChecker>`) so its future captures no external regions — see
//! `upstream.rs` for why.

use std::sync::Arc;

use crate::config::ServeConfig;
use crate::error::ProxyError;
use crate::owner::OwnerChecker;
use crate::proxy::classify::Rule;

/// Parse a buffered JSON body for rules that inspect it.
fn json_body(body: &[u8], what: &str) -> Result<serde_json::Value, ProxyError> {
    if body.is_empty() {
        return Err(ProxyError::bad_request(format!(
            "{what}: request body is required"
        )));
    }
    serde_json::from_slice(body)
        .map_err(|e| ProxyError::bad_request(format!("{what}: body is not valid JSON: {e}")))
}

fn required_str<'v>(
    v: &'v serde_json::Value,
    field: &str,
    what: &str,
) -> Result<&'v str, ProxyError> {
    v.get(field)
        .and_then(|f| f.as_str())
        .ok_or_else(|| ProxyError::bad_request(format!("{what}: missing string field `{field}`")))
}

/// Evaluate a classified REST write rule. `body` is the buffered request
/// body (empty for rules that do not inspect it). Ok(()) means forward.
pub async fn evaluate(
    rule: Rule,
    body: bytes::Bytes,
    config: ServeConfig,
    owner: Arc<OwnerChecker>,
) -> Result<(), ProxyError> {
    // 1. Org check — every write route.
    let org = match &rule {
        Rule::CreateIssue { org, .. }
        | Rule::CreatePullRequest { org, .. }
        | Rule::CreateComment { org, .. }
        | Rule::OwnerIssue { org, .. }
        | Rule::OwnerPull { org, .. }
        | Rule::OwnerIssueComment { org, .. }
        | Rule::OwnerPullComment { org, .. }
        | Rule::GitRefsCreate { org, .. }
        | Rule::GitRefsUpdate { org, .. }
        | Rule::GitObjects { org, .. }
        | Rule::Contents { org, .. } => org.clone(),
    };
    if !config.org_allowed(&org) {
        return Err(ProxyError::policy_denied(
            "org_allowlist",
            format!("organisation `{org}` is not in GAP_ALLOWED_ORGS"),
        ));
    }

    // 2. Rule-specific checks.
    match &rule {
        Rule::CreateIssue { .. } | Rule::CreateComment { .. } | Rule::GitObjects { .. } => Ok(()),

        Rule::CreatePullRequest { .. } => {
            let v = json_body(&body, "create pull request")?;
            let head = required_str(&v, "head", "create pull request")?;
            // `head` is either "branch" (same repo) or "owner:branch" (fork).
            let (head_owner, branch) = match head.split_once(':') {
                Some((o, b)) => (Some(o), b),
                None => (None, head),
            };
            if let Some(o) = head_owner {
                if !o.eq_ignore_ascii_case(owner.identity()) {
                    return Err(ProxyError::policy_denied(
                        "branch_prefix",
                        format!(
                            "cross-fork pull requests are only allowed from your own fork (head owner `{o}` != `{}`)",
                            owner.identity()
                        ),
                    ));
                }
            }
            if !config.branch_allowed(branch) {
                return Err(ProxyError::policy_denied(
                    "branch_prefix",
                    format!(
                        "pull request head branch `{branch}` does not start with `{}`",
                        config.branch_prefix
                    ),
                ));
            }
            Ok(())
        }

        Rule::GitRefsCreate { .. } => {
            let v = json_body(&body, "create ref")?;
            let git_ref = required_str(&v, "ref", "create ref")?;
            if !config.ref_allowed(git_ref) {
                return Err(ProxyError::policy_denied(
                    "branch_prefix",
                    format!(
                        "ref `{git_ref}` does not start with `{}` (tags and other branches are not permitted)",
                        config.branch_ref_prefix()
                    ),
                ));
            }
            Ok(())
        }

        Rule::GitRefsUpdate { refname, .. } => {
            let full = format!("refs/{refname}");
            if !config.ref_allowed(&full) {
                return Err(ProxyError::policy_denied(
                    "branch_prefix",
                    format!(
                        "ref `{full}` does not start with `{}`",
                        config.branch_ref_prefix()
                    ),
                ));
            }
            Ok(())
        }

        Rule::Contents { .. } => {
            let v = json_body(&body, "contents")?;
            // The branch field is mandatory: without it the contents API
            // writes to the repository's default branch.
            let branch = required_str(&v, "branch", "contents").map_err(|_| {
                ProxyError::policy_denied(
                    "branch_prefix",
                    "contents writes must name a `branch` explicitly (the default branch is never writable)",
                )
            })?;
            if !config.branch_allowed(branch) {
                return Err(ProxyError::policy_denied(
                    "branch_prefix",
                    format!(
                        "contents write targets branch `{branch}` which does not start with `{}`",
                        config.branch_prefix
                    ),
                ));
            }
            Ok(())
        }

        Rule::OwnerIssue { repo, number, .. } => {
            owner_only(owner, format!("/repos/{org}/{repo}/issues/{number}")).await
        }
        Rule::OwnerPull { repo, number, .. } => {
            owner_only(owner, format!("/repos/{org}/{repo}/pulls/{number}")).await
        }
        Rule::OwnerIssueComment { repo, id, .. } => {
            owner_only(owner, format!("/repos/{org}/{repo}/issues/comments/{id}")).await
        }
        Rule::OwnerPullComment { repo, id, .. } => {
            owner_only(owner, format!("/repos/{org}/{repo}/pulls/comments/{id}")).await
        }
    }
}

async fn owner_only(owner: Arc<OwnerChecker>, path: String) -> Result<(), ProxyError> {
    let identity = owner.identity().to_string();
    if owner.is_owner(path).await? {
        Ok(())
    } else {
        Err(ProxyError::policy_denied(
            "owner_only",
            format!("only the author ({identity}) may modify this resource"),
        ))
    }
}

/// LFS batch gate: `download` allowed, `upload` denied (v1). Synchronous —
/// it awaits nothing.
pub fn evaluate_lfs(org: &str, body: &[u8], config: &ServeConfig) -> Result<(), ProxyError> {
    if !config.org_allowed(org) {
        return Err(ProxyError::policy_denied(
            "org_allowlist",
            format!("organisation `{org}` is not in GAP_ALLOWED_ORGS"),
        ));
    }
    let v = json_body(body, "lfs batch")?;
    let operation = required_str(&v, "operation", "lfs batch")?;
    if operation != "download" {
        return Err(ProxyError::policy_denied(
            "lfs_upload",
            "LFS upload is not permitted (download only)",
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::proxy::classify::Rule;
    use bytes::Bytes;
    use clap::Parser;

    #[derive(clap::Parser)]
    struct TestCli {
        #[command(flatten)]
        serve: ServeConfig,
    }

    fn config() -> ServeConfig {
        TestCli::parse_from(["test", "--allowed-orgs", "myorg", "--token", "x"])
            .serve
            .normalized()
    }

    fn owner() -> Arc<OwnerChecker> {
        let client = reqwest::Client::new();
        Arc::new(OwnerChecker::new(
            client,
            "http://unused".into(),
            "x".into(),
            "agent-bot".into(),
            std::time::Duration::from_secs(60),
        ))
    }

    fn denied(result: Result<(), ProxyError>) -> ProxyError {
        match result {
            Err(e @ ProxyError::PolicyDenied { .. }) => e,
            other => panic!("expected policy denial, got {other:?}"),
        }
    }

    fn bytes(v: serde_json::Value) -> Bytes {
        Bytes::from(v.to_string())
    }

    #[tokio::test]
    async fn org_allowlist_applies_to_all_rules() {
        let rule = Rule::CreateIssue {
            org: "otherorg".into(),
            repo: "r".into(),
        };
        denied(evaluate(rule, Bytes::new(), config(), owner()).await);
    }

    #[tokio::test]
    async fn create_pr_head_prefix_enforced() {
        let rule = |o: &str| Rule::CreatePullRequest {
            org: o.into(),
            repo: "r".into(),
        };

        // same-repo branch, good prefix
        let ok =
            bytes(serde_json::json!({"title": "t", "head": "agent/fix-thing", "base": "main"}));
        assert!(evaluate(rule("myorg"), ok, config(), owner()).await.is_ok());

        // same-repo branch, bad prefix
        let bad = bytes(serde_json::json!({"title": "t", "head": "main", "base": "x"}));
        denied(evaluate(rule("myorg"), bad, config(), owner()).await);

        // fork form from own fork with prefix branch — allowed
        let own_fork = bytes(
            serde_json::json!({"title": "t", "head": "agent-bot:agent/work", "base": "main"}),
        );
        assert!(evaluate(rule("myorg"), own_fork, config(), owner())
            .await
            .is_ok());

        // fork form from someone else's fork — denied
        let other_fork =
            bytes(serde_json::json!({"title": "t", "head": "someone:agent/work", "base": "main"}));
        denied(evaluate(rule("myorg"), other_fork, config(), owner()).await);

        // missing head
        let no_head = bytes(serde_json::json!({"title": "t"}));
        assert!(matches!(
            evaluate(rule("myorg"), no_head, config(), owner()).await,
            Err(ProxyError::BadRequest(_))
        ));
    }

    #[tokio::test]
    async fn refs_rules() {
        let create = |o: &str| Rule::GitRefsCreate {
            org: o.into(),
            repo: "r".into(),
        };

        let ok = bytes(serde_json::json!({"ref": "refs/heads/agent/x", "sha": "a".repeat(40)}));
        assert!(evaluate(create("myorg"), ok, config(), owner())
            .await
            .is_ok());

        // tags blocked via create
        let tag = bytes(serde_json::json!({"ref": "refs/tags/v1.0.0", "sha": "a".repeat(40)}));
        denied(evaluate(create("myorg"), tag, config(), owner()).await);

        let update = Rule::GitRefsUpdate {
            org: "myorg".into(),
            repo: "r".into(),
            refname: "heads/agent/x".into(),
        };
        assert!(evaluate(update, Bytes::new(), config(), owner())
            .await
            .is_ok());

        let update_main = Rule::GitRefsUpdate {
            org: "myorg".into(),
            repo: "r".into(),
            refname: "heads/main".into(),
        };
        denied(evaluate(update_main, Bytes::new(), config(), owner()).await);
    }

    #[tokio::test]
    async fn contents_requires_prefixed_branch() {
        let rule = || Rule::Contents {
            org: "myorg".into(),
            repo: "r".into(),
        };

        let ok =
            bytes(serde_json::json!({"message": "m", "content": "aGk=", "branch": "agent/docs"}));
        assert!(evaluate(rule(), ok, config(), owner()).await.is_ok());

        // missing branch → policy denial (would write default branch)
        let no_branch = bytes(serde_json::json!({"message": "m", "content": "aGk="}));
        denied(evaluate(rule(), no_branch, config(), owner()).await);

        let main_branch =
            bytes(serde_json::json!({"message": "m", "content": "aGk=", "branch": "main"}));
        denied(evaluate(rule(), main_branch, config(), owner()).await);
    }

    #[test]
    fn lfs_download_only() {
        let cfg = config();
        let dl = serde_json::json!({"operation": "download", "objects": []});
        assert!(evaluate_lfs("myorg", dl.to_string().as_bytes(), &cfg).is_ok());

        let ul = serde_json::json!({"operation": "upload", "objects": []});
        denied(evaluate_lfs("myorg", ul.to_string().as_bytes(), &cfg));

        let dl_other_org = serde_json::json!({"operation": "download", "objects": []});
        denied(evaluate_lfs(
            "otherorg",
            dl_other_org.to_string().as_bytes(),
            &cfg,
        ));
    }
}
