//! End-to-end allow/deny matrix through a running proxy (plain HTTP,
//! wiremock upstream).

mod common;

use common::{client, start};
use wiremock::matchers::{method, path};
use wiremock::{Mock, ResponseTemplate};

const Z: &str = "0000000000000000000000000000000000000000";
const A: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";

fn pkt(data: &str) -> String {
    format!("{:04x}{data}", data.len() + 4)
}

async fn status_of_create_issue(proxy_base: &str, org: &str) -> u16 {
    client()
        .post(format!("{proxy_base}/api/v3/repos/{org}/r/issues"))
        .header("content-type", "application/json")
        .body(r#"{"title":"hi"}"#)
        .send()
        .await
        .unwrap()
        .status()
        .as_u16()
}

#[tokio::test]
async fn read_passthrough_injects_pat_and_strips_client_auth() {
    let t = start().await;
    Mock::given(method("GET"))
        .and(path("/repos/myorg/r"))
        .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({"id": 1})))
        .expect(1)
        .mount(&t.api)
        .await;

    let resp = client()
        .get(format!("{}/api/v3/repos/myorg/r", t.base))
        .header("authorization", "Bearer dummy-client-token")
        .header("cookie", "session=secret")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);

    let received = t.api.received_requests().await.unwrap();
    assert_eq!(received.len(), 1);
    let req = &received[0];
    // PAT injected, client auth replaced, cookie gone
    assert_eq!(
        req.headers.get("authorization").unwrap().to_str().unwrap(),
        "Bearer test-pat"
    );
    assert!(!req.headers.contains_key("cookie"));
}

#[tokio::test]
async fn create_issue_allowed_org_forwards() {
    let t = start().await;
    Mock::given(method("POST"))
        .and(path("/repos/myorg/r/issues"))
        .respond_with(ResponseTemplate::new(201).set_body_json(serde_json::json!({"number": 3})))
        .expect(1)
        .mount(&t.api)
        .await;
    assert_eq!(status_of_create_issue(&t.base, "myorg").await, 201);
}

#[tokio::test]
async fn create_issue_wrong_org_denied_without_upstream() {
    let t = start().await;
    let status = status_of_create_issue(&t.base, "evilorg").await;
    assert_eq!(status, 403);
    // default MockServer records everything; nothing should have arrived
    assert!(t.api.received_requests().await.unwrap().is_empty());
}

#[tokio::test]
async fn release_create_denied_even_in_allowed_org() {
    let t = start().await;
    let resp = client()
        .post(format!("{}/api/v3/repos/myorg/r/releases", t.base))
        .header("content-type", "application/json")
        .body(r#"{"tag_name":"v9.9.9"}"#)
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 403);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["rule"], "default_deny");
}

#[tokio::test]
async fn git_refs_tag_denied_branch_forwarded() {
    let t = start().await;
    Mock::given(method("POST"))
        .and(path("/repos/myorg/r/git/refs"))
        .respond_with(ResponseTemplate::new(201))
        .expect(1)
        .mount(&t.api)
        .await;

    // tag ref → 403
    let resp = client()
        .post(format!("{}/api/v3/repos/myorg/r/git/refs", t.base))
        .header("content-type", "application/json")
        .body(format!(r#"{{"ref":"refs/tags/v1","sha":"{A}"}}"#))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 403);

    // prefixed branch ref → forwarded
    let resp = client()
        .post(format!("{}/api/v3/repos/myorg/r/git/refs", t.base))
        .header("content-type", "application/json")
        .body(format!(r#"{{"ref":"refs/heads/agent/x","sha":"{A}"}}"#))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 201);
}

#[tokio::test]
async fn push_bad_ref_gets_synthetic_ng_no_upstream() {
    let t = start().await;
    let cmd = format!("{Z} {A} refs/heads/main");
    let body = format!("{}0000PACK", pkt(&cmd));
    let resp = client()
        .post(format!("{}/myorg/r.git/git-receive-pack", t.base))
        .header("content-type", "application/x-git-receive-pack-request")
        .body(body)
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200); // synthetic receive-pack result
    let text = resp.text().await.unwrap();
    assert!(text.contains("ng refs/heads/main"), "{text}");
    assert!(text.contains("policy:"), "{text}");
    assert!(t.api.received_requests().await.unwrap().is_empty());
}

#[tokio::test]
async fn push_good_ref_forwarded_byte_identical() {
    let t = start().await;
    Mock::given(method("POST"))
        .and(path("/myorg/r.git/git-receive-pack"))
        .respond_with(ResponseTemplate::new(200))
        .expect(1)
        .mount(&t.api)
        .await;

    let cmd = format!("{Z} {A} refs/heads/agent/feature");
    let body = format!("{}0000PACKDATA-binary-\u{1}\u{2}", pkt(&cmd));
    let resp = client()
        .post(format!("{}/myorg/r.git/git-receive-pack", t.base))
        .header("content-type", "application/x-git-receive-pack-request")
        .body(body.clone())
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);

    let received = t.api.received_requests().await.unwrap();
    assert_eq!(received[0].body, body.as_bytes());
}

#[tokio::test]
async fn push_wrong_org_denied() {
    let t = start().await;
    let cmd = format!("{Z} {A} refs/heads/agent/x");
    let body = format!("{}0000", pkt(&cmd));
    let resp = client()
        .post(format!("{}/evilorg/r.git/git-receive-pack", t.base))
        .header("content-type", "application/x-git-receive-pack-request")
        .body(body)
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 403);
}

#[tokio::test]
async fn lfs_download_allowed_upload_denied() {
    let t = start().await;
    Mock::given(method("POST"))
        .and(path("/myorg/r.git/info/lfs/objects/batch"))
        .respond_with(ResponseTemplate::new(200))
        .expect(1)
        .mount(&t.api)
        .await;

    let dl = client()
        .post(format!("{}/myorg/r.git/info/lfs/objects/batch", t.base))
        .header("content-type", "application/json")
        .body(r#"{"operation":"download","objects":[]}"#)
        .send()
        .await
        .unwrap();
    assert_eq!(dl.status(), 200);

    let ul = client()
        .post(format!("{}/myorg/r.git/info/lfs/objects/batch", t.base))
        .header("content-type", "application/json")
        .body(r#"{"operation":"upload","objects":[]}"#)
        .send()
        .await
        .unwrap();
    assert_eq!(ul.status(), 403);
}

#[tokio::test]
async fn graphql_query_allowed_mutation_denied() {
    let t = start().await;
    Mock::given(method("POST"))
        .and(path("/graphql"))
        .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({"data": {}})))
        .expect(1)
        .mount(&t.api)
        .await;

    let q = client()
        .post(format!("{}/api/graphql", t.base))
        .header("content-type", "application/json")
        .body(r#"{"query":"query { viewer { login } }"}"#)
        .send()
        .await
        .unwrap();
    assert_eq!(q.status(), 200);

    let m = client()
        .post(format!("{}/api/graphql", t.base))
        .header("content-type", "application/json")
        .body(r#"{"query":"mutation ($input: MinimizeCommentInput!) { minimizeComment(input: $input) { clientMutationId } }","variables":{"input":{"classifier":"SPAM","subjectId":"X"}}}"#)
        .send()
        .await
        .unwrap();
    assert_eq!(m.status(), 403);
}
