package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func policyForTest(t *testing.T, destinations, github string) *Policy {
	t.Helper()
	if destinations == "" {
		destinations = `{}`
	}
	if github == "" {
		github = `"allowed_orgs":["acme"],"branch_prefix":"agent/"`
	}
	p, err := parsePolicy([]byte(`{"https_mode":"intercept","destinations":` + destinations + `,"github":{` + github + `}}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDestinationExamples(t *testing.T) {
	for _, tt := range []struct {
		name, rules, host, ip string
		allow                 bool
	}{
		{"exact", `{"deny_hosts":["blocked.example"]}`, "BLOCKED.EXAMPLE.", "192.0.2.1", false},
		{"exact permits subdomain", `{"deny_hosts":["blocked.example"]}`, "sub.blocked.example", "192.0.2.1", true},
		{"wildcard", `{"deny_hosts":["*.blocked.example"]}`, "a.b.blocked.example", "192.0.2.1", false},
		{"wildcard apex", `{"deny_hosts":["*.blocked.example"]}`, "blocked.example", "192.0.2.1", true},
		{"wildcard boundary", `{"deny_hosts":["*.blocked.example"]}`, "notblocked.example", "192.0.2.1", true},
		{"regex", `{"deny_host_regexes":["tracking[0-9]+\\.example"]}`, "tracking12.example", "192.0.2.1", false},
		{"whole regex", `{"deny_host_regexes":["tracking[0-9]+\\.example"]}`, "nottracking12.example", "192.0.2.1", true},
		{"literal host IP", `{"deny_hosts":["192.168.20.12"],"deny_ips":[]}`, "192.168.20.12", "192.168.20.12", false},
		{"resolved host IP", `{"deny_hosts":["192.168.20.12"],"deny_ips":[]}`, "example.com", "192.168.20.12", true},
		{"IP literal", `{"deny_ips":["192.168.20.12"]}`, "192.168.20.12", "192.168.20.12", false},
		{"IP resolved", `{"deny_ips":["192.168.20.12"]}`, "example.com", "192.168.20.12", false},
		{"CIDR", `{"deny_ips":["198.51.100.0/24"]}`, "example.com", "198.51.100.9", false},
		{"outside CIDR", `{"deny_ips":["198.51.100.0/24"]}`, "example.com", "198.51.101.9", true},
		{"IPv6", `{"deny_ips":["2001:db8::/32"]}`, "example.com", "2001:db8::12", false},
		{"mapped IPv4", `{"deny_ips":["192.168.20.12"]}`, "::ffff:192.168.20.12", "::ffff:192.168.20.12", false},
		{"mapped CIDR", `{"deny_ips":["::ffff:192.168.20.0/120"]}`, "example.com", "192.168.20.12", false},
		{"literal IPv6 host", `{"deny_hosts":["2001:db8::12"]}`, "2001:db8:0:0::12", "2001:db8::12", false},
		{"shared IP unaffected", `{"deny_hosts":["blocked.example"]}`, "allowed.example", "192.0.2.1", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := &Proxy{config: Config{Policy: policyForTest(t, tt.rules, "")}}
			h, err := normalizeHost(tt.host)
			if err != nil {
				t.Fatal(err)
			}
			allow := p.checkHost(h) == nil && p.checkIP(netip.MustParseAddr(tt.ip), "443") == nil
			if allow != tt.allow {
				t.Fatalf("allowed=%v", allow)
			}
		})
	}
}

func TestCheckedDialDNS(t *testing.T) {
	p := &Proxy{config: Config{Policy: policyForTest(t, `{"deny_ips":["192.0.2.2"]}`, "")}}
	lookups, dials := 0, 0
	p.lookupIP = func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		return []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}, nil
	}
	p.dialContext = func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("unexpected dial")
	}
	if _, err := p.checkedDial(context.Background(), "tcp", "example.com:443"); !errors.Is(err, errDestinationDenied) || dials != 0 {
		t.Fatalf("mixed answers: %v, dials=%d", err, dials)
	}
	lookups = 0
	p.lookupIP = func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		if lookups > 1 {
			return []netip.Addr{netip.MustParseAddr("192.0.2.2")}, nil
		}
		return testLookup(context.Background(), "", "")
	}
	p.dialContext = func(_ context.Context, _, address string) (net.Conn, error) {
		if address != "192.0.2.1:443" {
			t.Fatalf("not pinned: %s", address)
		}
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}
	c, err := p.checkedDial(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if lookups != 1 {
		t.Fatal("resolved twice")
	}
	if _, err := p.checkedDial(context.Background(), "tcp", "example.com:443"); !errors.Is(err, errDestinationDenied) {
		t.Fatal("rebinding accepted", err)
	}
	for _, addr := range []string{"127.0.0.1:8443", "127.0.0.1:8444", "[::ffff:127.0.0.1]:8443", "[::1]:8444"} {
		if _, err := p.checkedDial(context.Background(), "tcp", addr); !errors.Is(err, errDestinationDenied) {
			t.Fatalf("proxy loop %s: %v", addr, err)
		}
	}
}

func TestPolicyValidation(t *testing.T) {
	good := `{"https_mode":"tunnel","github":{"allowed_users":["jacobfg"],"branch_prefix":"agp/"}}`
	for _, body := range []string{
		`{}`, `null`, good + `{}`, strings.Replace(good, `"tunnel"`, `"typo"`, 1),
		strings.Replace(good, `"github":`, `"extra":true,"github":`, 1),
		strings.Replace(good, `["jacobfg"]`, `null`, 1),
		strings.Replace(good, `"agp/"`, `"main"`, 1),
		strings.Replace(good, `"branch_prefix":`, `"allowed_users":[],"branch_prefix":`, 1),
		strings.Replace(good, `"branch_prefix":`, `"write_deny_repos":["zendesk/*"],"branch_prefix":`, 1),
		strings.Replace(good, `"github":`, `"destinations":{"deny_hosts":["https://example.com"]},"github":`, 1),
		strings.Replace(good, `"github":`, `"destinations":{"deny_host_regexes":["["]},"github":`, 1),
		strings.Replace(good, `"github":`, `"destinations":{"deny_ips":["example.com"]},"github":`, 1),
	} {
		if _, err := parsePolicy([]byte(body)); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	file := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(file, []byte(good), 0600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"POLICY_FILE": file, "GITHUB_TOKEN": "fake", "GITHUB_TOKEN_FILE": "", "TLS_CERT_FILE": "cert", "TLS_KEY_FILE": "key", "LISTEN_ADDR": "", "MAX_PUSH_BYTES": ""} {
		t.Setenv(key, value)
	}
	c, err := FromEnv()
	if err != nil || !c.Users["jacobfg"] || c.BranchPrefix != "agp/" {
		t.Fatalf("JSON config: %v", err)
	}
}

func TestRepositoryPolicyAcrossAPIs(t *testing.T) {
	for _, tt := range []struct {
		name, rules string
		allow       bool
	}{
		{"omitted", "", true}, {"empty", `,"write_allow_repos":[]`, false},
		{"allow", `,"write_allow_repos":["ACME/REPO"]`, true},
		{"other repo", `,"write_allow_repos":["acme/other"]`, false},
		{"deny wins", `,"write_allow_repos":["acme/repo"],"write_deny_repos":["acme/repo"]`, false},
		{"deny unrelated", `,"write_deny_repos":["acme/other"]`, true},
	} {
		for _, api := range []string{"REST", "GraphQL", "Git"} {
			t.Run(tt.name+"/"+api, func(t *testing.T) {
				p, writes := fixture(t)
				p.config.Policy = policyForTest(t, "", `"allowed_orgs":["acme"],"branch_prefix":"agent/"`+tt.rules)
				var code int
				switch api {
				case "REST":
					code = request(p, "POST", "/repos/acme/repo/issues", `{"title":"test"}`).Code
				case "GraphQL":
					b, _ := json.Marshal(map[string]string{"query": `mutation {createIssue(input:{repositoryId:"repo",title:"test"}){clientMutationId}}`})
					code = request(p, "POST", "/graphql", string(b)).Code
				case "Git":
					r, _ := http.NewRequest("POST", "https://github.com/acme/repo.git/git-receive-pack", strings.NewReader(packet(command("refs/heads/agent/test"))+"0000PACK"))
					r.Header.Set("Content-Type", "application/x-git-receive-pack-request")
					if err := p.authorizeGit(nil, r, r.URL.Path); err != nil {
						code = 403
					} else {
						code = 200
					}
				}
				if (code == 200) != tt.allow {
					t.Fatalf("status=%d", code)
				}
				if !tt.allow && writes.Load() != 0 {
					t.Fatal("blocked write forwarded")
				}
				if request(p, "GET", "/repos/acme/repo/issues", "").Code != 200 {
					t.Fatal("repository restriction filtered reads")
				}
			})
		}
	}
}

func TestCrossRepositoryPRPolicy(t *testing.T) {
	p, writes := fixture(t)
	p.config.Policy = policyForTest(t, "", `"allowed_orgs":["acme"],"branch_prefix":"agent/","write_allow_repos":["acme/repo"]`)
	original := p.client.Transport
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/pulls/1") {
			return response(map[string]any{"user": map[string]string{"login": "bot"}, "head": map[string]any{"ref": "agent/work", "repo": map[string]any{"name": "head", "owner": map[string]string{"type": "Organization", "login": "acme"}}}}), nil
		}
		if r.URL.Path == "/graphql" {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			var q struct {
				Query     string
				Variables map[string]any
			}
			json.Unmarshal(b, &q)
			if q.Query == nodeQuery && q.Variables["id"] == "head" {
				return response(map[string]any{"data": map[string]any{"node": map[string]any{"__typename": "Repository", "name": "head", "owner": map[string]string{"__typename": "Organization", "login": "acme"}}}}), nil
			}
		}
		return original.RoundTrip(r)
	})
	if request(p, "PATCH", "/repos/acme/repo/pulls/1", `{"body":"test"}`).Code != 403 {
		t.Fatal("REST head repo escaped allowlist")
	}
	b, _ := json.Marshal(map[string]string{"query": `mutation {createPullRequest(input:{repositoryId:"repo",headRepositoryId:"head",headRefName:"agent/work",baseRefName:"main",title:"test"}){clientMutationId}}`})
	if request(p, "POST", "/graphql", string(b)).Code != 403 {
		t.Fatal("GraphQL head repo escaped allowlist")
	}
	if writes.Load() != 0 {
		t.Fatal("denied PR forwarded")
	}
}

func TestDocumentedPolicyFile(t *testing.T) {
	b, err := os.ReadFile("../../examples/policy.json")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := parsePolicy(b)
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{config: Config{Policy: policy, Organizations: loginSet(policy.GitHub.AllowedOrgs), Users: loginSet(policy.GitHub.AllowedUsers)}}
	if !p.repositoryAllowed("User", "jacobfg", "permission-test") || p.repositoryAllowed("Organization", "zendesk", "protected") || p.repositoryAllowed("Organization", "zendesk", "other") {
		t.Fatal("documented repository policy differs")
	}
	if p.checkHost("192.168.20.12") == nil || p.checkHost("example.com") != nil || p.checkIP(netip.MustParseAddr("192.168.20.12"), "443") != nil {
		t.Fatal("documented literal-host IP distinction differs")
	}
}

func TestDestinationBlocksGitHubReadsAndStartup(t *testing.T) {
	p, writes := fixture(t)
	p.config.Policy = policyForTest(t, `{"deny_hosts":["api.github.com"]}`, "")
	if request(p, "GET", "/user", "").Code != 403 || writes.Load() != 0 {
		t.Fatal("GitHub read ignored destination rule")
	}
	p.config.Policy.HTTPSMode = "tunnel"
	_, err := newProxy(context.Background(), p.config, nil, func(p *Proxy, tr *http.Transport) {
		p.lookupIP = func(context.Context, string, string) ([]netip.Addr, error) {
			t.Fatal("blocked startup resolved DNS")
			return nil, nil
		}
	})
	if err == nil {
		t.Fatal("blocked GitHub identity startup succeeded")
	}
}
