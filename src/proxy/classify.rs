//! Request classification: read vs governed write vs default-deny.
//!
//! Classification is pure and cheap (method + path only). Rules that need a
//! request body are evaluated later, in `policy::rules`.

use http::Method;

/// A governed write operation with its path captures. Evaluation happens in
/// `policy::rules::evaluate`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Rule {
    /// POST /repos/{o}/{r}/issues
    CreateIssue { org: String, repo: String },
    /// POST /repos/{o}/{r}/pulls — body `head` must match the branch prefix.
    CreatePullRequest { org: String, repo: String },
    /// POST issue/PR comments and PR reviews.
    CreateComment { org: String, repo: String },
    /// PATCH/DELETE /repos/{o}/{r}/issues/{n}
    OwnerIssue {
        org: String,
        repo: String,
        number: u64,
    },
    /// PATCH/DELETE /repos/{o}/{r}/pulls/{n}
    OwnerPull {
        org: String,
        repo: String,
        number: u64,
    },
    /// PATCH/DELETE /repos/{o}/{r}/issues/comments/{id}
    OwnerIssueComment { org: String, repo: String, id: u64 },
    /// PATCH/DELETE /repos/{o}/{r}/pulls/comments/{id}
    OwnerPullComment { org: String, repo: String, id: u64 },
    /// POST /repos/{o}/{r}/git/refs — body `ref` must match the ref prefix.
    GitRefsCreate { org: String, repo: String },
    /// PATCH/DELETE /repos/{o}/{r}/git/refs/{ref} — path ref must match.
    GitRefsUpdate {
        org: String,
        repo: String,
        refname: String,
    },
    /// POST /repos/{o}/{r}/git/{blobs,trees,commits,tags} — dangling objects.
    GitObjects { org: String, repo: String },
    /// PUT/DELETE /repos/{o}/{r}/contents/{path} — body `branch` required and
    /// must match the branch prefix.
    Contents { org: String, repo: String },
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Classified {
    /// Reads are never filtered.
    Read,
    /// REST write governed by a rule.
    RestWrite(Rule),
    /// POST /{o}/{r}.git/git-receive-pack — handled by `policy::git_push`.
    GitReceivePack { org: String, repo: String },
    /// POST /{o}/{r}.git/info/lfs/objects/batch — download only.
    LfsBatch { org: String, repo: String },
    /// Write-ish request matching no allow rule.
    Denied,
}

fn parse_u64(s: &str) -> Option<u64> {
    s.parse().ok()
}

/// Percent-decode a path segment (git ref paths like `heads/agent%2Fx`).
fn decode_segment(s: &str) -> String {
    let bytes = s.as_bytes();
    let mut out: Vec<u8> = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' && i + 2 < bytes.len() {
            if let Ok(v) = u8::from_str_radix(&s[i + 1..i + 3], 16) {
                out.push(v);
                i += 3;
                continue;
            }
        }
        out.push(bytes[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

/// Classify an API-upstream request (path already normalized, no query).
pub fn classify_api(method: &Method, path: &str) -> Classified {
    let m = method.as_str();
    let read_only = matches!(m, "GET" | "HEAD" | "OPTIONS");
    let segs: Vec<&str> = path.trim_matches('/').split('/').collect();

    // Everything under /repos/{org}/{repo}/...
    if segs.len() >= 3 && segs[0] == "repos" {
        let org = segs[1].to_string();
        let repo = segs[2].to_string();
        let rest: &[&str] = &segs[3..];

        let rule = match (m, rest) {
            // issues
            ("POST", ["issues"]) => Some(Rule::CreateIssue {
                org: org.clone(),
                repo: repo.clone(),
            }),
            ("PATCH" | "DELETE", ["issues", n]) => parse_u64(n).map(|number| Rule::OwnerIssue {
                org: org.clone(),
                repo: repo.clone(),
                number,
            }),
            ("POST", ["issues", _, "comments"]) => Some(Rule::CreateComment {
                org: org.clone(),
                repo: repo.clone(),
            }),
            ("PATCH" | "DELETE", ["issues", "comments", id]) => {
                parse_u64(id).map(|id| Rule::OwnerIssueComment {
                    org: org.clone(),
                    repo: repo.clone(),
                    id,
                })
            }

            // pulls
            ("POST", ["pulls"]) => Some(Rule::CreatePullRequest {
                org: org.clone(),
                repo: repo.clone(),
            }),
            ("PATCH" | "DELETE", ["pulls", n]) => parse_u64(n).map(|number| Rule::OwnerPull {
                org: org.clone(),
                repo: repo.clone(),
                number,
            }),
            ("POST", ["pulls", _, "comments"]) => Some(Rule::CreateComment {
                org: org.clone(),
                repo: repo.clone(),
            }),
            ("POST", ["pulls", _, "reviews"]) => Some(Rule::CreateComment {
                org: org.clone(),
                repo: repo.clone(),
            }),
            ("PATCH" | "DELETE", ["pulls", "comments", id]) => {
                parse_u64(id).map(|id| Rule::OwnerPullComment {
                    org: org.clone(),
                    repo: repo.clone(),
                    id,
                })
            }

            // git refs and objects
            ("POST", ["git", "refs"]) => Some(Rule::GitRefsCreate {
                org: org.clone(),
                repo: repo.clone(),
            }),
            ("PATCH" | "DELETE", ["git", "refs", refpath @ ..]) => {
                let refname = decode_segment(&refpath.join("/"));
                Some(Rule::GitRefsUpdate {
                    org: org.clone(),
                    repo: repo.clone(),
                    refname,
                })
            }
            ("POST", ["git", kind]) if matches!(*kind, "blobs" | "trees" | "commits" | "tags") => {
                Some(Rule::GitObjects {
                    org: org.clone(),
                    repo: repo.clone(),
                })
            }

            // contents API
            ("PUT" | "DELETE", ["contents", ..]) => Some(Rule::Contents {
                org: org.clone(),
                repo: repo.clone(),
            }),

            _ => None,
        };

        if read_only {
            return Classified::Read;
        }
        return match rule {
            Some(r) => Classified::RestWrite(r),
            None => Classified::Denied,
        };
    }

    if read_only {
        Classified::Read
    } else {
        Classified::Denied
    }
}

/// Classify a git-upstream request (path with query still attached).
pub fn classify_git(method: &Method, path_and_query: &str) -> Classified {
    let m = method.as_str();
    let (path, query) = match path_and_query.find('?') {
        Some(i) => (&path_and_query[..i], &path_and_query[i + 1..]),
        None => (path_and_query, ""),
    };

    // /{org}/{repo}.git/...
    let segs: Vec<&str> = path.trim_matches('/').split('/').collect();
    if segs.len() >= 3 && segs[1].ends_with(".git") {
        let org = segs[0].to_string();
        let repo = segs[1].trim_end_matches(".git").to_string();
        let rest: &[&str] = &segs[2..];

        match (m, rest) {
            // upload-pack is fetch/clone: a read.
            ("POST", ["git-upload-pack"]) => return Classified::Read,
            ("POST", ["git-receive-pack"]) => return Classified::GitReceivePack { org, repo },
            ("POST", ["info", "lfs", "objects", "batch"]) => {
                return Classified::LfsBatch { org, repo }
            }
            // info/refs for either service is an advertisement: no state
            // changes, so it is a read regardless of the service param.
            ("GET", ["info", "refs"]) if query.contains("service=") => return Classified::Read,
            _ => {}
        }
    }

    if matches!(m, "GET" | "HEAD" | "OPTIONS") {
        Classified::Read
    } else {
        Classified::Denied
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reads_are_unfiltered() {
        assert_eq!(
            classify_api(&Method::GET, "/repos/anyone/anything"),
            Classified::Read
        );
        assert_eq!(
            classify_api(&Method::GET, "/users/octocat"),
            Classified::Read
        );
        assert_eq!(
            classify_git(&Method::GET, "/o/r.git/info/refs?service=git-receive-pack"),
            Classified::Read
        );
    }

    #[test]
    fn create_issue_and_pr() {
        assert_eq!(
            classify_api(&Method::POST, "/repos/o/r/issues"),
            Classified::RestWrite(Rule::CreateIssue {
                org: "o".into(),
                repo: "r".into()
            })
        );
        assert_eq!(
            classify_api(&Method::POST, "/repos/o/r/pulls"),
            Classified::RestWrite(Rule::CreatePullRequest {
                org: "o".into(),
                repo: "r".into()
            })
        );
    }

    #[test]
    fn owner_gated_routes() {
        assert_eq!(
            classify_api(&Method::PATCH, "/repos/o/r/issues/42"),
            Classified::RestWrite(Rule::OwnerIssue {
                org: "o".into(),
                repo: "r".into(),
                number: 42
            })
        );
        assert_eq!(
            classify_api(&Method::DELETE, "/repos/o/r/pulls/7"),
            Classified::RestWrite(Rule::OwnerPull {
                org: "o".into(),
                repo: "r".into(),
                number: 7
            })
        );
        assert_eq!(
            classify_api(&Method::PATCH, "/repos/o/r/issues/comments/99"),
            Classified::RestWrite(Rule::OwnerIssueComment {
                org: "o".into(),
                repo: "r".into(),
                id: 99
            })
        );
        assert_eq!(
            classify_api(&Method::DELETE, "/repos/o/r/pulls/comments/11"),
            Classified::RestWrite(Rule::OwnerPullComment {
                org: "o".into(),
                repo: "r".into(),
                id: 11
            })
        );
    }

    #[test]
    fn refs_and_contents() {
        assert_eq!(
            classify_api(&Method::POST, "/repos/o/r/git/refs"),
            Classified::RestWrite(Rule::GitRefsCreate {
                org: "o".into(),
                repo: "r".into()
            })
        );
        assert_eq!(
            classify_api(&Method::DELETE, "/repos/o/r/git/refs/heads/agent/x"),
            Classified::RestWrite(Rule::GitRefsUpdate {
                org: "o".into(),
                repo: "r".into(),
                refname: "heads/agent/x".into()
            })
        );
        assert_eq!(
            classify_api(&Method::PUT, "/repos/o/r/contents/README.md"),
            Classified::RestWrite(Rule::Contents {
                org: "o".into(),
                repo: "r".into()
            })
        );
        assert_eq!(
            classify_api(&Method::POST, "/repos/o/r/git/trees"),
            Classified::RestWrite(Rule::GitObjects {
                org: "o".into(),
                repo: "r".into()
            })
        );
    }

    #[test]
    fn default_deny_writes() {
        for (m, p) in [
            ("PATCH", "/repos/o/r"),
            ("DELETE", "/repos/o/r"),
            ("POST", "/repos/o/r/releases"),
            ("POST", "/repos/o/r/merges"),
            ("POST", "/repos/o/r/actions/workflows/ci.yml/dispatches"),
            ("POST", "/repos/o/r/issues/1/labels"),
            ("POST", "/repos/o/r/pulls/1/requested_reviewers"),
            ("POST", "/repos/o/r/statuses/abc123"),
            ("POST", "/repos/o/r/git/tags/abc"),
            ("POST", "/orgs/o/hooks"),
            ("POST", "/user/repos"),
        ] {
            let method = m.parse::<Method>().unwrap();
            assert_eq!(classify_api(&method, p), Classified::Denied, "{m} {p}");
        }
    }

    #[test]
    fn git_services() {
        assert_eq!(
            classify_git(&Method::POST, "/o/r.git/git-receive-pack"),
            Classified::GitReceivePack {
                org: "o".into(),
                repo: "r".into()
            }
        );
        assert_eq!(
            classify_git(&Method::POST, "/o/r.git/git-upload-pack"),
            Classified::Read
        );
        assert_eq!(
            classify_git(&Method::POST, "/o/r.git/info/lfs/objects/batch"),
            Classified::LfsBatch {
                org: "o".into(),
                repo: "r".into()
            }
        );
        // Non-repo writes on the git host are denied.
        assert_eq!(classify_git(&Method::POST, "/login"), Classified::Denied);
    }

    #[test]
    fn percent_encoded_ref() {
        assert_eq!(
            classify_api(&Method::DELETE, "/repos/o/r/git/refs/heads/agent%2Fx"),
            Classified::RestWrite(Rule::GitRefsUpdate {
                org: "o".into(),
                repo: "r".into(),
                refname: "heads/agent/x".into()
            })
        );
    }
}
