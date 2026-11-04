package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestConfiguredPolicy is an opt-in, offline report against the actual policy
// handler. Owner type and resource metadata are fixtures, never live lookups.
func TestConfiguredPolicy(t *testing.T) {
	if os.Getenv("POLICY_CHECK") != "1" {
		t.Skip("set POLICY_CHECK=1, POLICY_CHECK_REPO, POLICY_CHECK_OWNER_TYPE, ALLOWED_ORGS/ALLOWED_USERS, and BRANCH_PREFIX for an offline policy report")
	}
	repo := os.Getenv("POLICY_CHECK_REPO")
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_.-]+$`).MatchString(repo) {
		t.Fatal("POLICY_CHECK_REPO must be OWNER/REPO")
	}
	owner, name, _ := strings.Cut(repo, "/")
	if name == "." || name == ".." {
		t.Fatal("invalid repository name")
	}
	kind := os.Getenv("POLICY_CHECK_OWNER_TYPE")
	if kind != "User" && kind != "Organization" {
		t.Fatal("POLICY_CHECK_OWNER_TYPE must be User or Organization; this is supplied metadata, not inferred from the allowlists")
	}
	// Only policy inputs come from the environment. Never read an operator's
	// token file or use their PAT, listen address, or TLS material in this test.
	for key, value := range map[string]string{
		"GITHUB_TOKEN": "real-secret", "GITHUB_TOKEN_FILE": "",
		"TLS_CERT_FILE": "unused-test-cert", "TLS_KEY_FILE": "unused-test-key",
		"INTERCEPT_CA_CERT_FILE": "unused-test-ca", "INTERCEPT_CA_KEY_FILE": "unused-test-ca-key",
		"LISTEN_ADDR": "127.0.0.1:8443", "ALLOW_CONTAINER_BIND": "", "MAX_PUSH_BYTES": "",
	} {
		t.Setenv(key, value)
	}
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	listed := (&Proxy{config: c}).repositoryAllowed(kind, owner, name)
	branch := c.BranchPrefix + "work"
	t.Logf("OFFLINE: repository=%s owner_type=%s branch=%s; GitHub metadata is mocked, resource authors are bot or other, PAT identity is bot", repo, kind, branch)
	if !listed {
		t.Log("Repository owner is absent from its matching allowlist: all writes below should be BLOCKED")
	}

	type scenario struct {
		name, method, path, body string
		git, allow               bool
	}
	base := "/repos/" + repo
	jsonBody := func(v any) string {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	scenarios := []scenario{
		{"REST read", "GET", base, "", false, true},
		{"GraphQL read", "POST", "/graphql", jsonBody(map[string]any{"query": "query {viewer{login}}"}), false, true},
		{"create issue REST", "POST", base + "/issues", `{"title":"Policy check"}`, false, listed},
		{"edit own issue", "PATCH", base + "/issues/1", `{"body":"edit"}`, false, listed},
		{"edit another author's issue", "PATCH", base + "/issues/2", `{"body":"edit"}`, false, false},
		{"comment on own issue", "POST", base + "/issues/1/comments", `{"body":"comment"}`, false, listed},
		{"comment on another author's issue", "POST", base + "/issues/2/comments", `{"body":"comment"}`, false, false},
		{"create PR with allowed head", "POST", base + "/pulls", jsonBody(map[string]any{"head": branch, "base": "main", "title": "Policy check"}), false, listed},
		{"create PR with forbidden head", "POST", base + "/pulls", `{"head":"main","base":"main","title":"Policy check"}`, false, false},
		{"edit own PR", "PATCH", base + "/pulls/1", `{"body":"edit"}`, false, listed},
		{"edit another author's PR", "PATCH", base + "/pulls/2", `{"body":"edit"}`, false, false},
		{"edit PR with forbidden head", "PATCH", base + "/pulls/3", `{"body":"edit"}`, false, false},
		{"merge PR", "PUT", base + "/pulls/1/merge", `{}`, false, false},
		{"create branch ref", "POST", base + "/git/refs", jsonBody(map[string]any{"ref": "refs/heads/" + branch, "sha": strings.Repeat("1", 40)}), false, listed},
		{"force-update branch ref", "PATCH", base + "/git/refs/heads/" + branch, jsonBody(map[string]any{"sha": strings.Repeat("1", 40), "force": true}), false, listed},
		{"delete allowed branch ref", "DELETE", base + "/git/refs/heads/" + branch, "", false, listed},
		{"delete forbidden branch ref", "DELETE", base + "/git/refs/heads/main", "", false, false},
		{"write contents on allowed branch", "PUT", base + "/contents/file", jsonBody(map[string]any{"branch": branch, "content": "aGVsbG8=", "message": "edit"}), false, listed},
		{"write contents without explicit branch", "PUT", base + "/contents/file", `{"content":"aGVsbG8=","message":"edit"}`, false, false},
		{"rename within prefix", "POST", base + "/branches/" + branch + "/rename", jsonBody(map[string]any{"new_name": c.BranchPrefix + "renamed"}), false, listed},
		{"rename outside prefix", "POST", base + "/branches/" + branch + "/rename", `{"new_name":"main"}`, false, false},
		{"delete repository", "DELETE", base, "", false, false},
		{"Git clone/fetch advertisement", "GET", "/" + repo + ".git/info/refs?service=git-upload-pack", "", true, true},
		{"Git fetch RPC", "POST", "/" + repo + ".git/git-upload-pack", "fetch", true, true},
		{"Git push allowed branch", "POST", "/" + repo + ".git/git-receive-pack", packet(command("refs/heads/"+branch)) + "0000PACK", true, listed},
		{"Git force-push allowed branch", "POST", "/" + repo + ".git/git-receive-pack", packet(strings.Repeat("2", 40)+" "+strings.Repeat("1", 40)+" refs/heads/"+branch) + "0000PACK", true, listed},
		{"Git delete allowed branch", "POST", "/" + repo + ".git/git-receive-pack", packet(strings.Repeat("1", 40)+" "+strings.Repeat("0", 40)+" refs/heads/"+branch) + "0000", true, listed},
		{"Git push forbidden branch", "POST", "/" + repo + ".git/git-receive-pack", packet(command("refs/heads/main")) + "0000PACK", true, false},
		{"Git mixed allowed and forbidden push", "POST", "/" + repo + ".git/git-receive-pack", packet(command("refs/heads/"+branch)) + packet(command("refs/heads/main")) + "0000PACK", true, false},
		{"Git push tag", "POST", "/" + repo + ".git/git-receive-pack", packet(command("refs/tags/"+branch)) + "0000PACK", true, false},
		{"LFS download", "POST", "/" + repo + ".git/info/lfs/objects/batch", `{"operation":"download","objects":[]}`, true, true},
		{"LFS upload", "POST", "/" + repo + ".git/info/lfs/objects/batch", `{"operation":"upload","objects":[]}`, true, false},
	}
	for _, mutation := range []struct {
		name, field string
		input       map[string]any
		allow       bool
	}{
		{"create issue GraphQL", "createIssue", map[string]any{"repositoryId": "repo", "title": "Policy check"}, listed},
		{"delete own issue GraphQL", "deleteIssue", map[string]any{"issueId": "issue"}, listed},
		{"delete another author's issue GraphQL", "deleteIssue", map[string]any{"issueId": "other-issue"}, false},
		{"create PR GraphQL", "createPullRequest", map[string]any{"repositoryId": "repo", "headRefName": branch, "baseRefName": "main", "title": "Policy check"}, listed},
		{"edit own PR GraphQL", "updatePullRequest", map[string]any{"pullRequestId": "pr", "body": "edit"}, listed},
		{"edit another author's PR GraphQL", "updatePullRequest", map[string]any{"pullRequestId": "other-pr", "body": "edit"}, false},
		{"delete allowed ref GraphQL", "deleteRef", map[string]any{"refId": "ref"}, listed},
		{"delete forbidden ref GraphQL", "deleteRef", map[string]any{"refId": "main-ref"}, false},
	} {
		// Inline input values avoid needing a schema solely to name input types.
		var fields []string
		for key, value := range mutation.input {
			fields = append(fields, key+":"+jsonBody(value))
		}
		sort.Strings(fields)
		query := "mutation {" + mutation.field + "(input:{" + strings.Join(fields, ",") + "}){clientMutationId}}"
		scenarios = append(scenarios, scenario{mutation.name, "POST", "/graphql", jsonBody(map[string]any{"query": query}), false, mutation.allow})
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			p, writes := fixtureForOwner(t, owner, kind, name)
			p.config = c
			path := "/api" + s.path
			if s.git {
				path = "/git" + s.path
			}
			r := httptest.NewRequest(s.method, path, strings.NewReader(s.body))
			r.Host = "localhost"
			r.Header.Set("Content-Type", "application/json")
			if strings.HasSuffix(s.path, "/git-receive-pack") {
				r.Header.Set("Content-Type", "application/x-git-receive-pack-request")
			}
			res := httptest.NewRecorder()
			p.ServeHTTP(res, r)
			want := 200
			verdict := "ALLOW"
			if !s.allow {
				want = 403
				verdict = "BLOCK"
			}
			if res.Code != want {
				t.Fatalf("expected %s, got HTTP %d: %s", verdict, res.Code, res.Body.String())
			}
			if !s.allow && writes.Load() != 0 {
				t.Fatal("blocked write reached the simulated upstream")
			}
			t.Logf("%s | %s | HTTP %d", verdict, s.name, res.Code)
		})
	}
}
