//! Path normalization and upstream routing.
//!
//! Clients reach the proxy in two dialects:
//! - gh with GH_HOST set treats the proxy as an Enterprise host, prefixing
//!   REST paths with `/api/v3` and posting GraphQL to `/api/graphql`.
//! - git talks to `/{org}/{repo}.git/...` directly.
//!
//! Normalization maps both dialects onto the paths the real upstream expects.

/// Where a request is headed, and the path+query to send upstream.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Route {
    /// REST API upstream (default https://api.github.com).
    Api(String),
    /// GraphQL endpoint on the API upstream (upstream path `/graphql`).
    GraphQl,
    /// git/web upstream (default https://github.com).
    Git(String),
}

/// Split "path?query" into its parts; query is preserved for forwarding.
fn split_query(path_and_query: &str) -> (&str, &str) {
    match path_and_query.find('?') {
        Some(i) => (&path_and_query[..i], &path_and_query[i..]),
        None => (path_and_query, ""),
    }
}

/// Decide the upstream and normalize the path.
///
/// `ghes` preserves the `/api/v3` prefix on API routes instead of stripping
/// it (a real GitHub Enterprise Server expects it).
pub fn normalize(path_and_query: &str, ghes: bool) -> Route {
    let (path, query) = split_query(path_and_query);

    if path == "/api/graphql" {
        return Route::GraphQl;
    }
    if let Some(rest) = path.strip_prefix("/api/v3/") {
        let forwarded = if ghes {
            path.to_string()
        } else {
            format!("/{rest}")
        };
        return Route::Api(format!("{forwarded}{query}"));
    }
    if path == "/api/v3" {
        let forwarded = if ghes {
            path.to_string()
        } else {
            "/".to_string()
        };
        return Route::Api(format!("{forwarded}{query}"));
    }

    Route::Git(path_and_query.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn strips_api_v3_for_dotcom() {
        assert_eq!(
            normalize("/api/v3/repos/o/r/issues?state=open", false),
            Route::Api("/repos/o/r/issues?state=open".into())
        );
    }

    #[test]
    fn preserves_api_v3_for_ghes() {
        assert_eq!(
            normalize("/api/v3/repos/o/r/issues", true),
            Route::Api("/api/v3/repos/o/r/issues".into())
        );
    }

    #[test]
    fn maps_graphql() {
        assert_eq!(normalize("/api/graphql", false), Route::GraphQl);
    }

    #[test]
    fn git_paths_pass_through() {
        assert_eq!(
            normalize("/o/r.git/info/refs?service=git-receive-pack", false),
            Route::Git("/o/r.git/info/refs?service=git-receive-pack".into())
        );
    }
}
