package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOwnerAllowlists(t *testing.T) {
	for _, tt := range []struct {
		name, kind, orgs, users string
		allow                   bool
	}{
		{"organisation only", "Organization", "acme", "", true},
		{"user only", "User", "", "acme", true},
		{"both organisation", "Organization", "acme", "other", true},
		{"both user", "User", "other", "acme", true},
		{"organisation in wrong list", "Organization", "", "acme", false},
		{"user in wrong list", "User", "acme", "", false},
		{"unlisted user", "User", "other", "other", false},
		{"unknown owner type", "Bot", "acme", "acme", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, req := range []struct {
				method, path, body string
				eligible           bool
			}{
				{"POST", "/repos/acme/repo/issues", `{"title":"hello"}`, true},
				{"PATCH", "/repos/acme/repo/issues/1", `{"body":"edit"}`, true},
				{"PATCH", "/repos/acme/repo/issues/2", `{"body":"edit"}`, false},
				{"POST", "/repos/acme/repo/pulls", `{"head":"agent/work","base":"main"}`, true},
				{"PATCH", "/repos/acme/repo/pulls/1", `{"body":"edit"}`, true},
				{"PATCH", "/repos/acme/repo/pulls/2", `{"body":"edit"}`, false},
				{"PATCH", "/repos/acme/repo/pulls/3", `{"body":"edit"}`, false},
				{"DELETE", "/repos/acme/repo/git/refs/heads/agent/work", "", true},
				{"DELETE", "/repos/acme/repo/git/refs/heads/main", "", false},
			} {
				p, writes := fixtureForOwner(t, "acme", tt.kind)
				p.config.Organizations = loginSet(strings.Split(tt.orgs, ","))
				p.config.Users = loginSet(strings.Split(tt.users, ","))
				res := request(p, req.method, req.path, req.body)
				allowed := tt.allow && req.eligible
				if (res.Code == 200) != allowed || !allowed && writes.Load() != 0 {
					t.Fatalf("%s %s: %d %s", req.method, req.path, res.Code, res.Body.String())
				}
			}
			for _, mutation := range []struct {
				field, input string
				eligible     bool
			}{
				{"createIssue", `{repositoryId:"repo",title:"hello"}`, true},
				{"updateIssue", `{id:"issue",body:"edit"}`, true},
				{"updateIssue", `{id:"other-issue",body:"edit"}`, false},
				{"createPullRequest", `{repositoryId:"repo",headRefName:"agent/work",baseRefName:"main",title:"hello"}`, true},
				{"updatePullRequest", `{pullRequestId:"pr",body:"edit"}`, true},
				{"updatePullRequest", `{pullRequestId:"other-pr",body:"edit"}`, false},
				{"updatePullRequest", `{pullRequestId:"main-pr",body:"edit"}`, false},
				{"createRef", `{repositoryId:"repo",name:"refs/heads/agent/work",oid:"abc"}`, true},
				{"deleteRef", `{refId:"ref"}`, true},
				{"deleteRef", `{refId:"main-ref"}`, false},
			} {
				p, writes := fixtureForOwner(t, "acme", tt.kind)
				p.config.Organizations = loginSet(strings.Split(tt.orgs, ","))
				p.config.Users = loginSet(strings.Split(tt.users, ","))
				body, _ := json.Marshal(map[string]any{"query": "mutation {" + mutation.field + "(input:" + mutation.input + "){clientMutationId}}"})
				res := request(p, "POST", "/graphql", string(body))
				allowed := tt.allow && mutation.eligible
				if (res.Code == 200) != allowed || !allowed && writes.Load() != 0 {
					t.Fatalf("%s %s: %d %s", mutation.field, mutation.input, res.Code, res.Body.String())
				}
			}
			for _, branch := range []string{"agent/work", "main"} {
				p, writes := fixtureForOwner(t, "acme", tt.kind)
				p.config.Organizations = loginSet(strings.Split(tt.orgs, ","))
				p.config.Users = loginSet(strings.Split(tt.users, ","))
				r := httptest.NewRequest("POST", "/git/acme/repo.git/git-receive-pack", strings.NewReader(packet(command("refs/heads/"+branch))+"0000PACK"))
				r.Host = "localhost"
				r.Header.Set("Content-Type", "application/x-git-receive-pack-request")
				res := httptest.NewRecorder()
				p.ServeHTTP(res, r)
				allowed := tt.allow && branch == "agent/work"
				if (res.Code == 200) != allowed || !allowed && writes.Load() != 0 {
					t.Fatalf("push %s: %d %s", branch, res.Code, res.Body.String())
				}
			}
		})
	}
}
