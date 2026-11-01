//! Owner-only enforcement end-to-end: same-author writes forward, foreign
//! writes 403, repeat writes prove the author lookup is cached.

mod common;

use common::{client, start, IDENTITY};
use wiremock::matchers::{method, path};
use wiremock::{Mock, ResponseTemplate};

#[tokio::test]
async fn patch_own_issue_forwards_and_caches() {
    let t = start().await;
    Mock::given(method("GET"))
        .and(path("/repos/myorg/r/issues/42"))
        .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
            "user": {"login": IDENTITY}
        })))
        .expect(1) // second PATCH must hit the cache, not the upstream
        .mount(&t.api)
        .await;
    Mock::given(method("PATCH"))
        .and(path("/repos/myorg/r/issues/42"))
        .respond_with(ResponseTemplate::new(200))
        .expect(2)
        .mount(&t.api)
        .await;

    for _ in 0..2 {
        let resp = client()
            .patch(format!("{}/api/v3/repos/myorg/r/issues/42", t.base))
            .header("content-type", "application/json")
            .body(r#"{"title":"updated"}"#)
            .send()
            .await
            .unwrap();
        assert_eq!(resp.status(), 200);
    }
}

#[tokio::test]
async fn patch_foreign_issue_denied_and_not_forwarded() {
    let t = start().await;
    Mock::given(method("GET"))
        .and(path("/repos/myorg/r/issues/43"))
        .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
            "user": {"login": "someone-else"}
        })))
        .mount(&t.api)
        .await;

    let resp = client()
        .patch(format!("{}/api/v3/repos/myorg/r/issues/43", t.base))
        .header("content-type", "application/json")
        .body(r#"{"title":"hijack"}"#)
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 403);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["rule"], "owner_only");

    // The PATCH itself never reached the upstream.
    assert!(t
        .api
        .received_requests()
        .await
        .unwrap()
        .iter()
        .all(|r| r.method != http::Method::PATCH));
}

#[tokio::test]
async fn owner_lookup_failure_fails_closed() {
    let t = start().await;
    Mock::given(method("GET"))
        .and(path("/repos/myorg/r/issues/99"))
        .respond_with(ResponseTemplate::new(404))
        .mount(&t.api)
        .await;

    let resp = client()
        .delete(format!("{}/api/v3/repos/myorg/r/issues/99", t.base))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 403);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["rule"], "owner_check");
}

#[tokio::test]
async fn delete_own_pull_request_allowed() {
    let t = start().await;
    Mock::given(method("GET"))
        .and(path("/repos/myorg/r/pulls/7"))
        .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
            "user": {"login": IDENTITY}
        })))
        .mount(&t.api)
        .await;
    Mock::given(method("DELETE"))
        .and(path("/repos/myorg/r/pulls/7"))
        .respond_with(ResponseTemplate::new(204))
        .expect(1)
        .mount(&t.api)
        .await;

    let resp = client()
        .delete(format!("{}/api/v3/repos/myorg/r/pulls/7", t.base))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 204);
}
