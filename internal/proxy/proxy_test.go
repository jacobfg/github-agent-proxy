package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(body any) *http.Response {
	b, _ := json.Marshal(body)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(b))}
}

func fixture(t *testing.T) (*Proxy, *atomic.Int32) {
	t.Helper()
	return fixtureForOwner(t, "acme", "Organization")
}

func fixtureForOwner(t *testing.T, owner, ownerType string, names ...string) (*Proxy, *atomic.Int32) {
	t.Helper()
	repoName := "repo"
	if len(names) > 0 {
		repoName = names[0]
	}
	writes := &atomic.Int32{}
	p := &Proxy{config: Config{Token: "real-secret", Organizations: map[string]bool{}, Users: map[string]bool{}, BranchPrefix: "agent/", MaxPushBytes: 1 << 20}, login: "bot", tunnels: map[net.Conn]bool{}}
	if ownerType == "Organization" {
		p.config.Organizations[strings.ToLower(owner)] = true
	} else {
		p.config.Users[strings.ToLower(owner)] = true
	}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.github.com" && r.Header.Get("Authorization") != "Bearer real-secret" {
			t.Error("PAT not injected into API request")
		}
		if r.URL.Host == "github.com" {
			u, pw, ok := r.BasicAuth()
			if !ok || u != "x-access-token" || pw != "real-secret" {
				t.Error("Git credential not injected")
			}
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Http-Method-Override") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("client credential or override forwarded")
		}
		if r.URL.Path == "/graphql" {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			var q struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			_ = json.Unmarshal(b, &q)
			if q.Query == nodeQuery {
				id, _ := q.Variables["id"].(string)
				return response(map[string]any{"data": map[string]any{"node": testNode(id, owner, ownerType, p.config.BranchPrefix, repoName)}}), nil
			}
			if strings.Contains(q.Query, "repository(") {
				return response(map[string]any{"data": map[string]any{"repository": map[string]any{"id": "repo", "name": repoName, "nameWithOwner": owner + "/" + repoName, "owner": map[string]any{"login": owner}, "hasIssuesEnabled": true, "isBlankIssuesEnabled": true, "issueTemplates": []any{}, "viewerPermission": "ADMIN"}}}), nil
			}
			if strings.Contains(q.Query, "createIssue") {
				writes.Add(1)
				return response(map[string]any{"data": map[string]any{"createIssue": map[string]any{"issue": map[string]any{"url": "https://github.com/" + owner + "/" + repoName + "/issues/1", "number": 1}}}}), nil
			}
		}
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/") {
			path := r.URL.Path
			if strings.Contains(path, "/issues/") {
				author := "bot"
				if strings.HasSuffix(path, "/2") {
					author = "other"
				}
				issue := map[string]any{"user": map[string]any{"login": author}}
				if strings.HasSuffix(path, "/3") {
					issue["pull_request"] = map[string]any{}
				}
				return response(issue), nil
			}
			if strings.Contains(path, "/pulls/") {
				author := "bot"
				branch := p.config.BranchPrefix + "work"
				if strings.HasSuffix(path, "/2") {
					author = "other"
				}
				if strings.HasSuffix(path, "/3") {
					branch = "main"
				}
				return response(map[string]any{"user": map[string]any{"login": author}, "head": map[string]any{"ref": branch, "repo": map[string]any{"name": repoName, "owner": map[string]any{"login": owner, "type": ownerType}}}}), nil
			}
			typeName := ownerType
			parts := strings.Split(path, "/")
			return response(map[string]any{"name": parts[3], "owner": map[string]any{"login": parts[2], "type": typeName}}), nil
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			writes.Add(1)
		}
		var b []byte
		if r.Body != nil {
			b, _ = io.ReadAll(r.Body)
		}
		return response(map[string]any{"path": r.URL.Path, "body": string(b), "login": "bot"}), nil
	})
	p.transport = transport
	p.client = &http.Client{Transport: transport}
	return p, writes
}

func testNode(id, login, ownerType, branchPrefix string, names ...string) any {
	repoName := "repo"
	if len(names) > 0 {
		repoName = names[0]
	}
	owner := map[string]any{"__typename": ownerType, "login": login}
	repo := map[string]any{"name": repoName, "owner": owner}
	if id == "repo" {
		return map[string]any{"__typename": "Repository", "name": repoName, "owner": owner}
	}
	if id == "foreign-repo" {
		return map[string]any{"__typename": "Repository", "name": repoName, "owner": map[string]any{"__typename": "Organization", "login": "other"}}
	}
	if id == "ref" || id == "main-ref" || id == "tag" {
		name := branchPrefix + "work"
		prefix := "refs/heads/"
		if id == "main-ref" {
			name = "main"
		}
		if id == "tag" {
			prefix = "refs/tags/"
		}
		return map[string]any{"__typename": "Ref", "name": name, "prefix": prefix, "repository": repo}
	}
	if id == "missing" {
		return nil
	}
	kind := "Issue"
	author := "bot"
	branch := branchPrefix + "work"
	if strings.Contains(id, "pr") {
		kind = "PullRequest"
	}
	if strings.Contains(id, "other") {
		author = "other"
	}
	if strings.Contains(id, "main") {
		branch = "main"
	}
	return map[string]any{"__typename": kind, "author": map[string]any{"login": author}, "repository": repo, "headRefName": branch, "headRepository": repo}
}

func request(p *Proxy, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api"+path, strings.NewReader(body))
	r.Host = "localhost"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer fake")
	r.Header.Set("Cookie", "session=fake")
	r.Header.Set("Proxy-Authorization", "Basic fake")
	r.Header.Set("X-Http-Method-Override", "DELETE")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	return w
}

func TestRESTPolicy(t *testing.T) {
	tests := []struct {
		name, method, path, body string
		allow                    bool
	}{
		{"read any org", "GET", "/repos/other/repo/secrets", "", true},
		{"read any endpoint", "HEAD", "/user", "", true},
		{"render Markdown", "POST", "/markdown", `{"text":"hello","mode":"gfm","context":"other/repo"}`, true},
		{"new issue", "POST", "/repos/acme/repo/issues", `{"title":"hello"}`, true},
		{"foreign org", "POST", "/repos/other/repo/issues", `{"title":"hello"}`, false},
		{"edit own issue", "PATCH", "/repos/acme/repo/issues/1", `{"body":"edit"}`, true},
		{"edit other issue", "PATCH", "/repos/acme/repo/issues/2", `{"body":"edit"}`, false},
		{"PR through issues route", "PATCH", "/repos/acme/repo/issues/3", `{"body":"edit"}`, false},
		{"comment own issue", "POST", "/repos/acme/repo/issues/1/comments", `{"body":"hello"}`, true},
		{"comment other issue", "POST", "/repos/acme/repo/issues/2/comments", `{"body":"hello"}`, false},
		{"create PR", "POST", "/repos/acme/repo/pulls", `{"head":"agent/work","base":"main","title":"hello"}`, true},
		{"qualified PR head", "POST", "/repos/acme/repo/pulls", `{"head":"acme:agent/work","base":"main"}`, true},
		{"foreign PR head", "POST", "/repos/acme/repo/pulls", `{"head":"other:agent/work","base":"main"}`, false},
		{"protected PR head", "POST", "/repos/acme/repo/pulls", `{"head":"main","base":"main"}`, false},
		{"convert foreign issue", "POST", "/repos/acme/repo/pulls", `{"head":"agent/work","base":"main","issue":2}`, false},
		{"edit own PR", "PATCH", "/repos/acme/repo/pulls/1", `{"body":"edit"}`, true},
		{"edit other PR", "PATCH", "/repos/acme/repo/pulls/2", `{"body":"edit"}`, false},
		{"merge denied", "PUT", "/repos/acme/repo/pulls/1/merge", `{}`, false},
		{"new branch", "POST", "/repos/acme/repo/git/refs", `{"ref":"refs/heads/agent/work","sha":"abc"}`, true},
		{"tag denied", "POST", "/repos/acme/repo/git/refs", `{"ref":"refs/tags/agent/work","sha":"abc"}`, false},
		{"force branch", "PATCH", "/repos/acme/repo/git/refs/heads/agent/work", `{"sha":"abc","force":true}`, true},
		{"delete branch", "DELETE", "/repos/acme/repo/git/refs/heads/agent/work", "", true},
		{"delete main", "DELETE", "/repos/acme/repo/git/refs/heads/main", "", false},
		{"write content", "PUT", "/repos/acme/repo/contents/file", `{"branch":"agent/work","content":"aGVsbG8=","message":"edit"}`, true},
		{"default branch denied", "PUT", "/repos/acme/repo/contents/file", `{"content":"aGVsbG8=","message":"edit"}`, false},
		{"rename allowed", "POST", "/repos/acme/repo/branches/agent/work/rename", `{"new_name":"agent/new"}`, true},
		{"rename escape", "POST", "/repos/acme/repo/branches/agent/work/rename", `{"new_name":"main"}`, false},
		{"repo delete denied", "DELETE", "/repos/acme/repo", "", false},
		{"unknown field", "POST", "/repos/acme/repo/issues", `{"title":"hello","futureDanger":true}`, false},
		{"duplicate branch", "PUT", "/repos/acme/repo/contents/file", `{"branch":"main","branch":"agent/work"}`, false},
		{"trailing JSON", "POST", "/repos/acme/repo/issues", `{"title":"hello"}{}`, false},
		{"query override", "PATCH", "/repos/acme/repo/git/refs/heads/agent/work?ref=main", `{"sha":"abc"}`, false},
		{"path traversal", "DELETE", "/repos/acme/repo/git/refs/heads/agent/../main", "", false},
		{"double encoding", "DELETE", "/repos/acme/repo/git/refs/heads/agent/%252e%252e/main", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, writes := fixture(t)
			res := request(p, tt.method, tt.path, tt.body)
			if (res.Code == 200) != tt.allow {
				t.Fatalf("status %d: %s", res.Code, res.Body.String())
			}
			if !tt.allow && writes.Load() != 0 {
				t.Fatal("denied write reached GitHub")
			}
		})
	}
}

func TestGraphQLPolicy(t *testing.T) {
	tests := []struct {
		name, query string
		vars        map[string]any
		allow       bool
	}{
		{"read mentions mutation", `query { repository(owner:"other",name:"mutation deleteIssue") { name } }`, nil, true},
		{"create issue", `mutation($input:CreateIssueInput!){ createIssue(input:$input){clientMutationId}}`, map[string]any{"input": map[string]any{"repositoryId": "repo", "title": "hi"}}, true},
		{"default variable", `mutation($id:ID!="repo"){createIssue(input:{repositoryId:$id,title:"hi"}){clientMutationId}}`, nil, true},
		{"alias own issue", `mutation { renamed:updateIssue(input:{id:"issue",body:"edit"}){clientMutationId}}`, nil, true},
		{"other author", `mutation { updateIssue(input:{id:"other-issue",body:"edit"}){clientMutationId}}`, nil, false},
		{"delete own issue", `mutation {deleteIssue(input:{issueId:"issue"}){clientMutationId}}`, nil, true},
		{"foreign repo", `mutation {createIssue(input:{repositoryId:"foreign-repo",title:"hi"}){clientMutationId}}`, nil, false},
		{"new PR", `mutation {createPullRequest(input:{repositoryId:"repo",headRefName:"agent/work",baseRefName:"main",title:"hi"}){clientMutationId}}`, nil, true},
		{"bad PR branch", `mutation {createPullRequest(input:{repositoryId:"repo",headRefName:"main",baseRefName:"main",title:"hi"}){clientMutationId}}`, nil, false},
		{"foreign head repo", `mutation {createPullRequest(input:{repositoryId:"repo",headRepositoryId:"foreign-repo",headRefName:"agent/work",baseRefName:"main",title:"hi"}){clientMutationId}}`, nil, false},
		{"edit PR", `mutation {updatePullRequest(input:{pullRequestId:"pr",body:"hi"}){clientMutationId}}`, nil, true},
		{"edit PR wrong head", `mutation {updatePullRequest(input:{pullRequestId:"main-pr",body:"hi"}){clientMutationId}}`, nil, false},
		{"merge denied", `mutation {mergePullRequest(input:{pullRequestId:"pr"}){clientMutationId}}`, nil, false},
		{"comment on other", `mutation {addComment(input:{subjectId:"other-issue",body:"hi"}){clientMutationId}}`, nil, false},
		{"branch delete", `mutation {deleteRef(input:{refId:"ref"}){clientMutationId}}`, nil, true},
		{"main delete", `mutation {deleteRef(input:{refId:"main-ref"}){clientMutationId}}`, nil, false},
		{"tag delete", `mutation {deleteRef(input:{refId:"tag"}){clientMutationId}}`, nil, false},
		{"fragment", `mutation {...Edit} fragment Edit on Mutation {closeIssue(input:{issueId:"issue"}){clientMutationId}}`, nil, true},
		{"fragment hides denied", `mutation {...Edit} fragment Edit on Mutation {deleteRepository(input:{repositoryId:"repo"}){clientMutationId}}`, nil, false},
		{"cycle", `mutation {...A} fragment A on Mutation {...A}`, nil, false},
		{"mixed mutations", `mutation { a:closeIssue(input:{issueId:"issue"}){clientMutationId} b:closeIssue(input:{issueId:"other-issue"}){clientMutationId}}`, nil, false},
		{"skip cannot hide", `mutation {deleteRepository(input:{repositoryId:"repo"}) @skip(if:true){clientMutationId}}`, nil, false},
		{"duplicate input", `mutation {deleteRef(input:{refId:"main-ref",refId:"ref"}){clientMutationId}}`, nil, false},
		{"duplicate argument", `mutation {deleteRef(input:{refId:"main-ref"},input:{refId:"ref"}){clientMutationId}}`, nil, false},
		{"ambiguous operation", `query Read {viewer{login}} mutation Write {deleteRef(input:{refId:"ref"}){clientMutationId}}`, nil, false},
		{"missing node", `mutation {deleteIssue(input:{issueId:"missing"}){clientMutationId}}`, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, writes := fixture(t)
			b, _ := json.Marshal(map[string]any{"query": tt.query, "variables": tt.vars})
			res := request(p, "POST", "/graphql", string(b))
			if (res.Code == 200) != tt.allow {
				t.Fatalf("status %d: %s", res.Code, res.Body.String())
			}
			if !tt.allow && writes.Load() != 0 {
				t.Fatal("denied mutation forwarded")
			}
		})
	}
	p, _ := fixture(t)
	q := url.QueryEscape(`mutation {deleteRef(input:{refId:"ref"}){clientMutationId}}`)
	if request(p, "GET", "/graphql?query="+q, "").Code != 403 {
		t.Fatal("GET mutation allowed")
	}
	b := `{"query":"query Read {viewer{login}} mutation Write {deleteRef(input:{refId:\"main-ref\"}){clientMutationId}}","operationName":"Read"}`
	if res := request(p, "POST", "/graphql", b); res.Code != 200 {
		t.Fatal(res.Body.String())
	}
}

func packet(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
func command(ref string) string {
	return strings.Repeat("0", 40) + " " + strings.Repeat("1", 40) + " " + ref
}

func TestPushCommands(t *testing.T) {
	tests := []struct {
		name, wire string
		allow      bool
	}{
		{"create", packet(command("refs/heads/agent/work")+"\x00report-status side-band-64k object-format=sha1 agent=git/2.50") + "0000PACK", true},
		{"capability whitespace", packet(command("refs/heads/agent/work")+"\x00 report-status side-band-64k ") + "0000PACK", true},
		{"shallow create", packet("shallow "+strings.Repeat("a", 40)+"\n") + packet(command("refs/heads/agent/work")+"\x00report-status side-band-64k") + "0000PACK", true},
		{"multiple shallow boundaries", packet("shallow "+strings.Repeat("a", 40)+"\n") + packet("shallow "+strings.Repeat("b", 40)+"\n") + packet(command("refs/heads/agent/work")) + "0000PACK", true},
		{"shallow protected ref", packet("shallow "+strings.Repeat("a", 40)+"\n") + packet(command("refs/heads/main")) + "0000PACK", false},
		{"shallow mixed refs", packet("shallow "+strings.Repeat("a", 40)+"\n") + packet(command("refs/heads/agent/work")) + packet(command("refs/heads/main")) + "0000PACK", false},
		{"shallow without command", packet("shallow "+strings.Repeat("a", 40)+"\n") + "0000", false},
		{"shallow invalid object", packet("shallow "+strings.Repeat("z", 40)+"\n") + packet(command("refs/heads/agent/work")) + "0000", false},
		{"shallow zero object", packet("shallow "+strings.Repeat("0", 40)+"\n") + packet(command("refs/heads/agent/work")) + "0000", false},
		{"shallow capabilities", packet("shallow "+strings.Repeat("a", 40)+"\x00report-status") + packet(command("refs/heads/agent/work")) + "0000", false},
		{"late shallow boundary", packet(command("refs/heads/agent/work")) + packet("shallow "+strings.Repeat("a", 40)+"\n") + "0000", false},
		{"delete", packet(strings.Repeat("1", 40)+" "+strings.Repeat("0", 40)+" refs/heads/agent/work") + "0000", true},
		{"force", packet(command("refs/heads/agent/work")) + "0000PACK", true},
		{"main", packet(command("refs/heads/main")) + "0000", false},
		{"mixed", packet(command("refs/heads/agent/work")) + packet(command("refs/heads/main")) + "0000", false},
		{"tag", packet(command("refs/tags/agent/work")) + "0000", false},
		{"near prefix", packet(command("refs/heads/agentish/work")) + "0000", false},
		{"push options", packet(command("refs/heads/agent/work")+"\x00push-options") + "0000", false},
		{"signed push", packet("push-cert\x00report-status") + "0000", false},
		{"truncated", "0010abc", false},
		{"invalid length", "nope", false},
		{"special packet", "0001", false},
		{"duplicate ref", packet(command("refs/heads/agent/work")) + packet(command("refs/heads/agent/work")) + "0000", false},
		{"late capabilities", packet(command("refs/heads/agent/work")) + packet(command("refs/heads/agent/other")+"\x00report-status") + "0000", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _ := fixture(t)
			_, err := p.readCommands(strings.NewReader(tt.wire))
			if (err == nil) != tt.allow {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func testCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "proxy test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost", "api.github.com", "github.com"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	// Match mkcert's trust chain: the server presents a leaf signed by a
	// separate CA, and clients receive only the public CA certificate.
	leaf := *template
	leaf.SerialNumber = big.NewInt(2)
	leaf.Subject = pkix.Name{CommonName: "proxy test server"}
	leaf.IsCA = false
	leaf.KeyUsage = x509.KeyUsageDigitalSignature
	der, err = x509.CreateCertificate(rand.Reader, &leaf, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert, rootPEM
}

func TestHTTPSConnect(t *testing.T) {
	p, _ := fixture(t)
	cert, certPEM := testCertificate(t)
	p.tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	s := httptest.NewUnstartedServer(p)
	s.TLS = p.tlsConfig
	s.StartTLS()
	defer s.Close()
	defer p.Close()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	endpoint, _ := url.Parse(s.URL)
	transport := &http.Transport{Proxy: http.ProxyURL(endpoint), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: false}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	for _, host := range []string{"api.github.com", "github.com"} {
		r, _ := http.NewRequestWithContext(context.Background(), "GET", "https://"+host+"/user", nil)
		r.Header.Set("Authorization", "Bearer fake")
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%d %s", res.StatusCode, b)
		}
	}
	if res, err := client.Get("https://github.com:8443/"); err == nil {
		res.Body.Close()
		t.Fatal("GitHub on an alternate port allowed")
	}
	r, _ := http.NewRequest("POST", "https://api.github.com/repos/acme/repo/issues", strings.NewReader(`{"title":"hello"}`))
	r.Header.Set("Content-Type", "application/json")
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.Status)
	}
}

func TestHostAndEncodingGuards(t *testing.T) {
	for _, raw := range []string{"https://evil.example/api/user", "https://localhost/api/repos/acme/repo/../repo/issues"} {
		p, _ := fixture(t)
		r := httptest.NewRequest("GET", raw, nil)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatal("unsafe host/path accepted")
		}
	}
	p, _ := fixture(t)
	r := httptest.NewRequest("POST", "/api/repos/acme/repo/issues", strings.NewReader(`{"title":"hi"}`))
	r.Host = "localhost"
	r.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatal(w.Code)
	}
}

func TestReadRPCs(t *testing.T) {
	for _, tt := range []struct {
		path, body string
		allow      bool
	}{
		{"/git/other/repo.git/git-upload-pack", "fetch", true},
		{"/git/other/repo.git/info/lfs/objects/batch", `{"operation":"download","objects":[]}`, true},
		{"/git/acme/repo.git/info/lfs/objects/batch", `{"operation":"upload","objects":[]}`, false},
	} {
		p, _ := fixture(t)
		r := httptest.NewRequest("POST", tt.path, strings.NewReader(tt.body))
		r.Host = "localhost"
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if (w.Code == 200) != tt.allow {
			t.Fatalf("%s: %d %s", tt.path, w.Code, w.Body.String())
		}
	}
}

func TestMetadataFailureDeniesWrites(t *testing.T) {
	for _, body := range []string{`{"errors":[{"message":"unavailable"}]}`, `{"data":{"node":null}}`, `not json`} {
		p, writes := fixture(t)
		p.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		for _, req := range []struct{ path, body string }{
			{"/repos/acme/repo/issues", `{"title":"hello"}`},
			{"/graphql", `{"query":"mutation {deleteRef(input:{refId:\"ref\"}){clientMutationId}}"}`},
		} {
			if res := request(p, "POST", req.path, req.body); res.Code != 403 {
				t.Fatalf("%s: %d", body, res.Code)
			}
		}
		if writes.Load() != 0 {
			t.Fatal("write forwarded after failed lookup")
		}
	}
}

func TestStartupIdentityAndRedirectSafety(t *testing.T) {
	cert, certPEM := testCertificate(t)
	var hits atomic.Int32
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer redirect.Close()
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer startup-secret" {
			t.Error("unexpected upstream auth or host")
		}
		if r.URL.Path == "/user" {
			_, _ = io.WriteString(w, `{"login":"token-owner"}`)
			return
		}
		http.Redirect(w, r, redirect.URL, http.StatusFound)
	}))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	s.StartTLS()
	defer s.Close()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	p, err := newProxy(context.Background(), Config{Token: "startup-secret"}, nil, func(p *Proxy, transport *http.Transport) {
		transport.TLSClientConfig.RootCAs = roots
		p.lookupIP = testLookup
		p.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, s.Listener.Addr().String())
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.login != "token-owner" {
		t.Fatal("identity was not resolved")
	}
	var result any
	if p.api(context.Background(), "GET", "/redirect", nil, &result) == nil || hits.Load() != 0 {
		t.Fatal("authenticated metadata redirect was followed")
	}
}

func FuzzPushCommands(f *testing.F) {
	f.Add(packet(command("refs/heads/agent/work")) + "0000")
	f.Add("0001")
	p := &Proxy{config: Config{BranchPrefix: "agent/"}}
	f.Fuzz(func(t *testing.T, wire string) {
		header, err := p.readCommands(strings.NewReader(wire))
		if err == nil && (!strings.HasPrefix(wire, string(header)) || !bytes.HasSuffix(header, []byte("0000"))) {
			t.Fatal("invalid replay prefix")
		}
	})
}
