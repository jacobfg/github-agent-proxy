package proxy

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPolicyReadOnly(t *testing.T) {
	file := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(file, []byte(DefaultPolicy()), 0600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"POLICY_FILE": file, "GITHUB_TOKEN": "real-secret", "GITHUB_TOKEN_FILE": "",
		"ALLOWED_ORGS": "", "ALLOWED_USERS": "", "BRANCH_PREFIX": "",
		"TLS_CERT_FILE": "cert", "TLS_KEY_FILE": "key", "LISTEN_ADDR": "",
		"INTERCEPT_CA_CERT_FILE": "", "INTERCEPT_CA_KEY_FILE": "", "MAX_PUSH_BYTES": "",
	} {
		t.Setenv(k, v)
	}
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.interceptAll() || len(c.Users) != 0 || len(c.Organizations) != 0 || c.Policy.GitHub.WriteAllowRepos == nil || len(c.Policy.GitHub.WriteAllowRepos) != 0 {
		t.Fatal("default must use tunnel mode and empty owner/repository allowlists")
	}
	if d := c.Policy.Destinations; len(d.DenyHosts)+len(d.DenyHostRegexes)+len(d.DenyIPs) != 0 {
		t.Fatal("default blocks destinations")
	}
	for _, variant := range []string{"default", "only owner added", "only repository added", "ownerless legacy"} {
		t.Run(variant, func(t *testing.T) {
			p, writes := fixture(t)
			p.config = c
			policy := *c.Policy
			p.config.Policy = &policy
			switch variant {
			case "only owner added":
				p.config.Organizations = map[string]bool{"acme": true}
			case "only repository added":
				policy.GitHub.WriteAllowRepos = []string{"acme/repo"}
			case "ownerless legacy":
				p.config.Policy = nil
			}
			if request(p, "POST", "/repos/acme/repo/issues", `{"title":"test"}`).Code != http.StatusForbidden {
				t.Fatal("REST write permitted")
			}
			if request(p, "POST", "/graphql", `{"query":"mutation {createIssue(input:{repositoryId:\"repo\",title:\"test\"}){clientMutationId}}"}`).Code != http.StatusForbidden {
				t.Fatal("GraphQL write permitted")
			}
			r, err := http.NewRequest("POST", "https://github.com/acme/repo.git/git-receive-pack", strings.NewReader(packet(command("refs/heads/agent/test"))+"0000PACK"))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Content-Type", "application/x-git-receive-pack-request")
			if err := p.authorizeGit(nil, r, r.URL.Path); err == nil {
				t.Fatal("Git push permitted")
			}
			if writes.Load() != 0 {
				t.Fatal("denied write reached upstream")
			}
			if request(p, "GET", "/repos/acme/repo/issues", "").Code != http.StatusOK || request(p, "POST", "/graphql", `{"query":"query {viewer{login}}"}`).Code != http.StatusOK {
				t.Fatal("reads blocked")
			}
		})
	}
}
