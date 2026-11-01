//! GraphQL enforcement.
//!
//! gh performs `issue create`, `pr create`, `comment`, `close`, etc. as
//! GraphQL *mutations* — a REST-only allowlist would make gh read-only in
//! practice. Queries pass unfiltered; mutations are limited to a small
//! allowlist, each with the same org / branch-prefix / owner checks as the
//! REST rules. Anything unparseable or ambiguous fails closed.
//!
//! Scope notes (v1, documented):
//! - mutation inputs are read from `variables` (gh always uses variables);
//!   inline literal inputs are supported for scalars/objects too.
//! - comment mutations (IssueComment / PullRequestReviewComment) expose no
//!   repository via the node API, so they are enforced owner-only; the PAT's
//!   own repository scoping is the org backstop for those.
//!
//! Structure note: parsing/planning is fully synchronous and the AST never
//! crosses an `.await` — graphql-parser types carry a lifetime-bound `Text`
//! well-formedness bound that fails rustc's generalization inside opaque
//! futures. `evaluate` plans owned `MutationCheck`s, drops the document,
//! and only then performs (region-free, owned-argument) lookups.

use std::sync::Arc;
use std::time::Duration;

use graphql_parser::parse_query;
use graphql_parser::query::{Definition, Field, OperationDefinition, Selection, Value};
use moka::future::Cache;

use crate::config::ServeConfig;
use crate::error::ProxyError;

/// Mutations that modify an existing object: allowed only when the object's
/// author is the PAT owner. (Review comments have no GraphQL delete; REST
/// covers them.)
const OWNER_ONLY_MUTATIONS: [&str; 9] = [
    "updateIssue",
    "updatePullRequest",
    "closeIssue",
    "reopenIssue",
    "updateIssueComment",
    "updatePullRequestComment",
    "updatePullRequestReviewComment",
    "deleteIssueComment",
    "deleteIssue",
];

/// The input field holding the target object ID varies by mutation
/// (CloseIssueInput.issueId, UpdatePullRequestInput.pullRequestId, ...).
fn owner_id_field(mutation: &str) -> &'static str {
    match mutation {
        "closeIssue" | "reopenIssue" | "deleteIssue" => "issueId",
        "updatePullRequest" => "pullRequestId",
        "updatePullRequestComment" => "pullRequestCommentId",
        "updatePullRequestReviewComment" => "pullRequestReviewCommentId",
        // updateIssue, updateIssueComment, deleteIssueComment
        _ => "id",
    }
}

const NODE_QUERY: &str = r#"query ($id: ID!) {
  node(id: $id) {
    __typename
    ... on Issue { author { login } repository { nameWithOwner } }
    ... on PullRequest { author { login } repository { nameWithOwner} }
    ... on IssueComment { author { login } }
    ... on PullRequestReviewComment { author { login } }
    ... on Repository { nameWithOwner }
  }
}"#;

#[derive(Clone, Debug, Default)]
struct NodeInfo {
    author: Option<String>,
    name_with_owner: Option<String>,
}

/// An async check the sync planner extracts from a mutation document.
#[derive(Debug, PartialEq, Eq)]
enum MutationCheck {
    /// Resolve the node, require its repository's org to be allowed.
    OrgOfNode(String),
    /// Resolve the node, require its author to be the PAT owner.
    OwnerOfNode(String),
}

pub struct GraphQlPolicy {
    client: reqwest::Client,
    graphql_url: String,
    token: String,
    nodes: Cache<String, NodeInfo>,
}

impl GraphQlPolicy {
    pub fn new(client: reqwest::Client, graphql_url: String, token: String, ttl: Duration) -> Self {
        let nodes = Cache::builder()
            .time_to_live(ttl)
            .max_capacity(10_000)
            .build();
        Self {
            client,
            graphql_url,
            token,
            nodes,
        }
    }

    /// Evaluate a buffered GraphQL request body. Ok(()) means forward.
    pub async fn evaluate(
        self: Arc<Self>,
        body: bytes::Bytes,
        config: ServeConfig,
        identity: String,
    ) -> Result<(), ProxyError> {
        let checks = plan_mutations(&body, &config)?;
        for check in checks {
            match check {
                MutationCheck::OrgOfNode(id) => {
                    let node = self.clone().node(id).await?;
                    self.org_check(&node, &config)?;
                }
                MutationCheck::OwnerOfNode(id) => {
                    let node = self.clone().node(id).await?;
                    let author = node.author.as_deref().ok_or_else(|| {
                        ProxyError::policy_denied(
                            "owner_check",
                            "could not verify authorship of the mutation target",
                        )
                    })?;
                    if !author.eq_ignore_ascii_case(&identity) {
                        return Err(ProxyError::policy_denied(
                            "owner_only",
                            format!("only the author ({identity}) may modify this resource"),
                        ));
                    }
                }
            }
        }
        Ok(())
    }

    fn org_check(&self, node: &NodeInfo, config: &ServeConfig) -> Result<(), ProxyError> {
        let nwo = node.name_with_owner.as_deref().ok_or_else(|| {
            ProxyError::policy_denied(
                "org_allowlist",
                "the mutation target's repository could not be resolved",
            )
        })?;
        let org = nwo.split('/').next().unwrap_or("");
        if !config.org_allowed(org) {
            return Err(ProxyError::policy_denied(
                "org_allowlist",
                format!("repository `{nwo}` is not in an allowed organisation"),
            ));
        }
        Ok(())
    }

    async fn node(self: Arc<Self>, id: String) -> Result<NodeInfo, ProxyError> {
        self.nodes
            .try_get_with(id.clone(), self.clone().fetch_node(id))
            .await
            .map_err(|e: Arc<ProxyError>| {
                Arc::try_unwrap(e)
                    .unwrap_or_else(|e| ProxyError::policy_denied("owner_check", e.to_string()))
            })
    }

    async fn fetch_node(self: Arc<Self>, id: String) -> Result<NodeInfo, ProxyError> {
        let resp = self
            .client
            .post(&self.graphql_url)
            .bearer_auth(&self.token)
            .json(&serde_json::json!({
                "query": NODE_QUERY,
                "variables": { "id": id },
            }))
            .send()
            .await
            .map_err(|e| ProxyError::upstream(format!("graphql node lookup failed: {e}")))?;
        if !resp.status().is_success() {
            return Err(ProxyError::policy_denied(
                "owner_check",
                "could not resolve the mutation target",
            ));
        }
        let v: serde_json::Value = resp
            .json()
            .await
            .map_err(|e| ProxyError::upstream(format!("graphql node lookup not JSON: {e}")))?;
        if v.get("errors").is_some() {
            return Err(ProxyError::policy_denied(
                "owner_check",
                "could not resolve the mutation target",
            ));
        }
        let node = &v["data"]["node"];
        if node.is_null() {
            return Err(ProxyError::policy_denied(
                "owner_check",
                "could not resolve the mutation target",
            ));
        }
        let name_with_owner = node
            .pointer("/repository/nameWithOwner")
            .or_else(|| node.get("nameWithOwner"))
            .and_then(|v| v.as_str())
            .map(str::to_string);
        Ok(NodeInfo {
            author: node
                .pointer("/author/login")
                .and_then(|v| v.as_str())
                .map(str::to_string),
            name_with_owner,
        })
    }
}

/// Synchronously plan the checks a GraphQL document requires. Queries pass
/// with no checks; mutations produce node lookups or fail immediately.
fn plan_mutations(body: &[u8], config: &ServeConfig) -> Result<Vec<MutationCheck>, ProxyError> {
    let envelope: serde_json::Value = serde_json::from_slice(body)
        .map_err(|e| ProxyError::bad_request(format!("graphql: body is not JSON: {e}")))?;
    let query = envelope
        .get("query")
        .and_then(|q| q.as_str())
        .ok_or_else(|| ProxyError::bad_request("graphql: missing `query`"))?;
    let variables = envelope
        .get("variables")
        .cloned()
        .unwrap_or(serde_json::json!({}));
    let operation_name = envelope.get("operationName").and_then(|n| n.as_str());

    let doc = parse_query::<&str>(query)
        .map_err(|_| ProxyError::policy_denied("graphql_parse", "unparseable GraphQL document"))?;

    let mut operations: Vec<&OperationDefinition<&str>> = Vec::new();
    for def in &doc.definitions {
        if let Definition::Operation(op) = def {
            operations.push(op);
        }
    }
    if operations.is_empty() {
        return Err(ProxyError::policy_denied(
            "graphql_parse",
            "document contains no operation",
        ));
    }
    let selected: Vec<&&OperationDefinition<&str>> = if operations.len() == 1 {
        operations.iter().collect()
    } else {
        let name = operation_name.ok_or_else(|| {
            ProxyError::policy_denied(
                "graphql_parse",
                "multi-operation documents require operationName",
            )
        })?;
        operations
            .iter()
            .filter(|op| operation_name_of(op) == Some(name))
            .collect()
    };
    if selected.is_empty() {
        return Err(ProxyError::policy_denied(
            "graphql_parse",
            "operationName does not match any operation",
        ));
    }

    let mut checks = Vec::new();
    for op in selected {
        match op {
            // Reads are unfiltered.
            OperationDefinition::Query(_) | OperationDefinition::SelectionSet(_) => {}
            OperationDefinition::Subscription(_) => {
                return Err(ProxyError::policy_denied(
                    "graphql_mutation",
                    "subscriptions are not permitted",
                ));
            }
            OperationDefinition::Mutation(m) => {
                for sel in &m.selection_set.items {
                    let Selection::Field(field) = sel else {
                        return Err(ProxyError::policy_denied(
                            "graphql_mutation",
                            "fragment spreads at mutation root are not supported",
                        ));
                    };
                    plan_mutation_field(field, &variables, config, &mut checks)?;
                }
            }
        }
    }
    Ok(checks)
}

fn plan_mutation_field<'d, 't: 'd>(
    field: &'d Field<'t, &'t str>,
    variables: &'d serde_json::Value,
    config: &ServeConfig,
    checks: &mut Vec<MutationCheck>,
) -> Result<(), ProxyError> {
    let name = field.name;
    match name {
        "createIssue" => {
            let input = mutation_input(field, variables)?;
            let repo_id = input_str(&input, "repositoryId", "createIssue")?;
            checks.push(MutationCheck::OrgOfNode(repo_id.to_string()));
            Ok(())
        }
        "addComment" => {
            let input = mutation_input(field, variables)?;
            let subject_id = input_str(&input, "subjectId", "addComment")?;
            checks.push(MutationCheck::OrgOfNode(subject_id.to_string()));
            Ok(())
        }
        "createPullRequest" => {
            let input = mutation_input(field, variables)?;
            let head = input_str(&input, "headRefName", "createPullRequest")?;
            if !config.branch_allowed(head) {
                return Err(ProxyError::policy_denied(
                    "branch_prefix",
                    format!(
                        "pull request head branch `{head}` does not start with `{}`",
                        config.branch_prefix
                    ),
                ));
            }
            let repo_id = input_str(&input, "repositoryId", "createPullRequest")?;
            checks.push(MutationCheck::OrgOfNode(repo_id.to_string()));
            Ok(())
        }
        n if OWNER_ONLY_MUTATIONS.contains(&n) => {
            let input = mutation_input(field, variables)?;
            let id = input_str(&input, owner_id_field(n), n)?;
            checks.push(MutationCheck::OwnerOfNode(id.to_string()));
            Ok(())
        }
        other => Err(ProxyError::policy_denied(
            "graphql_mutation",
            format!("mutation `{other}` is not allowlisted"),
        )),
    }
}

fn operation_name_of<'d, 't: 'd>(op: &'d OperationDefinition<'t, &'t str>) -> Option<&'t str> {
    match op {
        OperationDefinition::Query(q) => q.name,
        OperationDefinition::Mutation(m) => m.name,
        OperationDefinition::Subscription(s) => s.name,
        OperationDefinition::SelectionSet(_) => None,
    }
}

/// Extract the mutation's `input` argument, resolving variables.
fn mutation_input<'d, 't: 'd>(
    field: &'d Field<'t, &'t str>,
    variables: &'d serde_json::Value,
) -> Result<serde_json::Value, ProxyError> {
    for (arg_name, value) in &field.arguments {
        if *arg_name == "input" {
            return Ok(ast_to_json(value, variables));
        }
    }
    Err(ProxyError::policy_denied(
        "graphql_mutation",
        format!("mutation `{}` has no `input` argument", field.name),
    ))
}

fn input_str<'v>(
    input: &'v serde_json::Value,
    field: &str,
    mutation: &str,
) -> Result<&'v str, ProxyError> {
    input.get(field).and_then(|v| v.as_str()).ok_or_else(|| {
        ProxyError::policy_denied(
            "graphql_mutation",
            format!("mutation `{mutation}` input is missing `{field}`"),
        )
    })
}

fn ast_to_json<'d, 't: 'd>(
    value: &'d Value<'t, &'t str>,
    variables: &'d serde_json::Value,
) -> serde_json::Value {
    match value {
        Value::Variable(name) => variables
            .get(*name)
            .cloned()
            .unwrap_or(serde_json::Value::Null),
        Value::Int(i) => i
            .as_i64()
            .map(serde_json::Value::from)
            .unwrap_or(serde_json::Value::Null),
        Value::Float(f) => serde_json::json!(f),
        Value::String(s) => serde_json::Value::String(s.to_string()),
        Value::Boolean(b) => serde_json::Value::Bool(*b),
        Value::Null => serde_json::Value::Null,
        Value::Enum(e) => serde_json::Value::String(e.to_string()),
        Value::List(items) => {
            serde_json::Value::Array(items.iter().map(|v| ast_to_json(v, variables)).collect())
        }
        Value::Object(map) => serde_json::Value::Object(
            map.iter()
                .map(|(k, v)| (k.to_string(), ast_to_json(v, variables)))
                .collect(),
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use bytes::Bytes;
    use clap::Parser;
    use wiremock::matchers::{method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    fn config() -> ServeConfig {
        #[derive(clap::Parser)]
        struct TestCli {
            #[command(flatten)]
            serve: ServeConfig,
        }
        TestCli::parse_from(["test", "--allowed-orgs", "myorg", "--token", "x"])
            .serve
            .normalized()
    }

    async fn policy() -> (Arc<GraphQlPolicy>, MockServer) {
        let server = MockServer::start().await;
        let p = Arc::new(GraphQlPolicy::new(
            reqwest::Client::new(),
            format!("{}/graphql", server.uri()),
            "x".into(),
            Duration::from_secs(60),
        ));
        (p, server)
    }

    fn envelope(query: &str, variables: serde_json::Value) -> Bytes {
        Bytes::from(serde_json::json!({"query": query, "variables": variables}).to_string())
    }

    fn identity() -> String {
        "agent-bot".to_string()
    }

    #[test]
    fn owner_only_mutations_use_their_id_fields() {
        // CloseIssueInput.issueId (not id) — the case gh issue close hits.
        let body = envelope(
            "mutation ($input: CloseIssueInput!) { closeIssue(input: $input) { issue { id } } }",
            serde_json::json!({"input": {"issueId": "I_1"}}),
        );
        let checks = plan_mutations(&body, &config()).unwrap();
        assert_eq!(checks, vec![MutationCheck::OwnerOfNode("I_1".to_string())]);

        // UpdatePullRequestInput.pullRequestId
        let body = envelope(
            "mutation ($input: UpdatePullRequestInput!) { updatePullRequest(input: $input) { pullRequest { id } } }",
            serde_json::json!({"input": {"pullRequestId": "PR_1", "title": "t"}}),
        );
        let checks = plan_mutations(&body, &config()).unwrap();
        assert_eq!(checks, vec![MutationCheck::OwnerOfNode("PR_1".to_string())]);
    }

    #[tokio::test]
    async fn queries_are_allowed_without_lookups() {
        let (p, _server) = policy().await;
        let body = envelope("query { viewer { login } }", serde_json::json!({}));
        assert!(p.evaluate(body, config(), identity()).await.is_ok());
    }

    #[tokio::test]
    async fn unparseable_is_denied() {
        let (p, _server) = policy().await;
        let body = envelope("mutation { (((", serde_json::json!({}));
        assert!(matches!(
            p.evaluate(body, config(), identity()).await,
            Err(ProxyError::PolicyDenied {
                rule: "graphql_parse",
                ..
            })
        ));
    }

    #[tokio::test]
    async fn non_allowlisted_mutation_is_denied() {
        let (p, _server) = policy().await;
        let body = envelope(
            "mutation ($input: AddLabelsToLabelableInput!) { addLabelsToLabelable(input: $input) { clientMutationId } }",
            serde_json::json!({"input": {"labelableId": "I_x", "labelIds": []}}),
        );
        assert!(matches!(
            p.evaluate(body, config(), identity()).await,
            Err(ProxyError::PolicyDenied {
                rule: "graphql_mutation",
                ..
            })
        ));
    }

    #[tokio::test]
    async fn create_pr_enforces_head_prefix_before_lookup() {
        let (p, _server) = policy().await;
        let body = envelope(
            "mutation ($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { id } } }",
            serde_json::json!({"input": {"repositoryId": "R_1", "headRefName": "main", "baseRefName": "main", "title": "t"}}),
        );
        assert!(matches!(
            p.evaluate(body, config(), identity()).await,
            Err(ProxyError::PolicyDenied {
                rule: "branch_prefix",
                ..
            })
        ));
    }

    #[tokio::test]
    async fn create_pr_ok_when_prefix_and_org_match() {
        let (p, server) = policy().await;
        Mock::given(method("POST"))
            .and(path("/graphql"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "data": {"node": {"__typename": "Repository", "nameWithOwner": "myorg/repo"}}
            })))
            .mount(&server)
            .await;
        let body = envelope(
            "mutation ($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { id } } }",
            serde_json::json!({"input": {"repositoryId": "R_1", "headRefName": "agent/work", "baseRefName": "main", "title": "t"}}),
        );
        assert!(p.evaluate(body, config(), identity()).await.is_ok());
    }

    #[tokio::test]
    async fn owner_only_mutation_checks_author() {
        let (p, server) = policy().await;
        Mock::given(method("POST"))
            .and(path("/graphql"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "data": {"node": {"__typename": "Issue", "author": {"login": "someone-else"}, "repository": {"nameWithOwner": "myorg/repo"}}}
            })))
            .mount(&server)
            .await;
        let body = envelope(
            "mutation ($input: UpdateIssueInput!) { updateIssue(input: $input) { issue { id } } }",
            serde_json::json!({"input": {"id": "I_1", "title": "retitled"}}),
        );
        assert!(matches!(
            p.evaluate(body, config(), identity()).await,
            Err(ProxyError::PolicyDenied {
                rule: "owner_only",
                ..
            })
        ));
    }

    #[tokio::test]
    async fn multi_operation_requires_operation_name() {
        let (p, _server) = policy().await;
        let body = envelope(
            "query A { viewer { login } } query B { viewer { login } }",
            serde_json::json!({}),
        );
        assert!(matches!(
            p.evaluate(body, config(), identity()).await,
            Err(ProxyError::PolicyDenied {
                rule: "graphql_parse",
                ..
            })
        ));
    }

    #[tokio::test]
    async fn inline_input_objects_are_supported() {
        let (p, server) = policy().await;
        Mock::given(method("POST"))
            .and(path("/graphql"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "data": {"node": {"__typename": "Repository", "nameWithOwner": "otherorg/repo"}}
            })))
            .mount(&server)
            .await;
        let body = envelope(
            r#"mutation { createIssue(input: {repositoryId: "R_1", title: "hi"}) { issue { id } } }"#,
            serde_json::json!({}),
        );
        // org check fires against the resolved repository → denied for otherorg
        assert!(matches!(
            p.evaluate(body, config(), identity()).await,
            Err(ProxyError::PolicyDenied {
                rule: "org_allowlist",
                ..
            })
        ));
    }
}
