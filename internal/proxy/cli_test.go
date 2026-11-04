package proxy

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func cleanCLIEnv(home string) []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if upper == "HOME" || strings.HasPrefix(upper, "GH_") || strings.HasPrefix(upper, "GITHUB_") || strings.HasPrefix(upper, "GIT_") || strings.HasPrefix(upper, "SSL_") || strings.HasSuffix(upper, "_PROXY") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
}

func cliServer(t *testing.T, p *Proxy) (string, string) {
	t.Helper()
	cert, certPEM := testCertificate(t)
	p.tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			p.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, r)
		if rec.Code >= 400 {
			t.Logf("%s %s: %d %s", r.Method, r.URL.Path, rec.Code, rec.Body.String())
		}
		for key, values := range rec.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(rec.Code)
		_, _ = io.Copy(w, rec.Body)
	}))
	s.TLS = p.tlsConfig
	s.StartTLS()
	t.Cleanup(func() { p.Close(); s.Close() })
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	return s.URL, ca
}

func TestGitCLI(t *testing.T) {
	for _, owner := range []struct{ login, kind string }{{"acme", "Organization"}, {"jacobfg", "User"}} {
		t.Run(owner.kind, func(t *testing.T) { testGitCLI(t, owner.login, owner.kind) })
	}
}

func testGitCLI(t *testing.T, owner, ownerType string, intercept ...bool) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	env := cleanCLIEnv(t.TempDir())
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, owner, "repo.git")
	if err = os.MkdirAll(filepath.Dir(bare), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = env
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	run(root, "init", "--bare", bare)
	run(root, "init", "-b", "main", work)
	if err = os.WriteFile(filepath.Join(work, "hello.txt"), []byte("hello\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run(work, "add", ".")
	run(work, "commit", "-m", "seed")
	seed := run(work, "rev-parse", "HEAD")
	run(work, "push", bare, "main")
	run(root, "--git-dir="+bare, "symbolic-ref", "HEAD", "refs/heads/main")
	run(root, "--git-dir="+bare, "config", "http.receivepack", "true")
	backend := httptest.NewServer(&cgi.Handler{Path: git, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}})
	defer backend.Close()
	backendURL, _ := url.Parse(backend.URL)
	p, _ := fixtureForOwner(t, owner, ownerType)
	var interceptCA []byte
	if len(intercept) > 0 && intercept[0] {
		interceptCA = enableInterception(t, p)
		p.config.Policy.GitHub.WriteAllowRepos = []string{owner + "/repo"}
	}
	metadata := p.transport
	direct := http.DefaultTransport.(*http.Transport).Clone()
	direct.Proxy = nil
	defer direct.CloseIdleConnections()
	p.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "github.com" {
			return metadata.RoundTrip(r)
		}
		u, password, ok := r.BasicAuth()
		if !ok || u != "x-access-token" || password != "real-secret" {
			t.Error("Git PAT auth missing")
		}
		out := r.Clone(r.Context())
		out.URL.Scheme = backendURL.Scheme
		out.URL.Host = backendURL.Host
		out.Host = backendURL.Host
		return direct.RoundTrip(out)
	})
	endpoint, ca := cliServer(t, p)
	if len(interceptCA) > 0 {
		f, err := os.OpenFile(ca, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.Write(interceptCA)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	env = append(env, "GIT_SSL_CAINFO="+ca)
	remote := "https://github.com/" + owner + "/repo.git"
	t.Run("HTTPSProxyCA", func(t *testing.T) {
		// gh repo clone also invokes Git, so both the outer HTTPS proxy and
		// the GitHub TLS connection inside CONNECT must trust the local CA.
		cmd := exec.Command(git, "-c", "http.proxySSLCAInfo="+ca, "ls-remote", remote)
		cmd.Env = append(append([]string{}, env...), "HTTPS_PROXY="+endpoint, "SSL_CERT_FILE="+ca)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git through HTTPS proxy: %v\n%s", err, b)
		}
	})
	config := []string{"-c", "http.https://github.com/.proxy=" + endpoint, "-c", "http.proxySSLCAInfo=" + ca, "-c", "credential.helper="}
	push := func(allow bool, args ...string) {
		t.Helper()
		argv := append(append([]string{}, config...), "push", remote)
		argv = append(argv, args...)
		cmd := exec.Command(git, argv...)
		cmd.Dir = work
		cmd.Env = env
		b, e := cmd.CombinedOutput()
		if (e == nil) != allow {
			t.Fatalf("push %v: %v\n%s", args, e, b)
		}
	}
	clone := filepath.Join(root, "clone")
	run(root, append(append([]string{}, config...), "clone", remote, clone)...)
	if got := run(clone, "rev-parse", "HEAD"); got != seed {
		t.Fatal("clone did not fetch main")
	}
	// A shallow client sends boundary declarations before its ref commands.
	shallow := filepath.Join(root, "shallow")
	run(root, append(append([]string{}, config...), "clone", "--depth=1", remote, shallow)...)
	if got := run(shallow, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Fatal("expected a shallow clone")
	}
	shallowPush := exec.Command(git, append(append([]string{}, config...), "push", remote, "HEAD:refs/heads/agent/shallow")...)
	shallowPush.Dir = shallow
	shallowPush.Env = env
	if b, err := shallowPush.CombinedOutput(); err != nil {
		t.Fatalf("push from shallow clone: %v\n%s", err, b)
	}
	if got := run(root, "--git-dir="+bare, "rev-parse", "refs/heads/agent/shallow"); got != seed {
		t.Fatal("shallow push did not create the requested ref")
	}
	run(work, "commit", "--allow-empty", "-m", "agent change")
	push(true, "HEAD:refs/heads/agent/work")
	first := run(root, "--git-dir="+bare, "rev-parse", "refs/heads/agent/work")
	run(work, "commit", "--amend", "--allow-empty", "-m", "rewritten agent change")
	push(true, "--force", "HEAD:refs/heads/agent/work")
	if got := run(root, "--git-dir="+bare, "rev-parse", "refs/heads/agent/work"); got == first {
		t.Fatal("force push did not change ref")
	}
	push(false, "--force", "HEAD:refs/heads/main")
	push(false, "--force", "HEAD:refs/heads/agent/batch", "HEAD:refs/heads/main")
	cmd := exec.Command(git, "--git-dir="+bare, "show-ref", "--verify", "refs/heads/agent/batch")
	cmd.Env = env
	if cmd.Run() == nil {
		t.Fatal("part of a rejected push was applied")
	}
	push(false, "HEAD:refs/tags/agent/tag")
	push(true, "--delete", "agent/work")
	cmd = exec.Command(git, "--git-dir="+bare, "show-ref", "--verify", "refs/heads/agent/work")
	cmd.Env = env
	if cmd.Run() == nil {
		t.Fatal("branch delete did not apply")
	}
	if got := run(root, "--git-dir="+bare, "rev-parse", "refs/heads/main"); got != seed {
		t.Fatal("protected main changed")
	}
	blocked, _ := fixtureForOwner(t, owner, ownerType)
	blocked.config.Policy = &Policy{HTTPSMode: "tunnel", GitHub: GitHubPolicy{WriteDenyRepos: []string{owner + "/repo"}}}
	blocked.transport = p.transport
	blockedEndpoint, blockedCA := cliServer(t, blocked)
	env = append(env, "GIT_SSL_CAINFO="+blockedCA)
	blockedArgs := []string{"-c", "http.https://github.com/.proxy=" + blockedEndpoint, "-c", "http.proxySSLCAInfo=" + blockedCA, "-c", "http.sslCAInfo=" + blockedCA}
	run(root, append(append([]string{}, blockedArgs...), "ls-remote", remote)...)
	denyPush := exec.Command(git, append(blockedArgs, "push", remote, "HEAD:refs/heads/agent/blocked")...)
	denyPush.Dir = work
	denyPush.Env = env
	if b, err := denyPush.CombinedOutput(); err == nil || !strings.Contains(string(b), "403") {
		t.Fatalf("repo-denied push: %v %s", err, b)
	}
}

func TestGHCLI(t *testing.T) {
	for _, owner := range []struct{ login, kind string }{{"acme", "Organization"}, {"jacobfg", "User"}} {
		t.Run(owner.kind, func(t *testing.T) { testGHCLI(t, owner.login, owner.kind) })
	}
}

func testGHCLI(t *testing.T, owner, ownerType string, intercept ...bool) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		t.Skip("gh is not installed")
	}
	p, _ := fixtureForOwner(t, owner, ownerType)
	var interceptCA []byte
	if len(intercept) > 0 && intercept[0] {
		interceptCA = enableInterception(t, p)
		p.config.Policy.GitHub.WriteAllowRepos = []string{owner + "/repo"}
	}
	endpoint, ca := cliServer(t, p)
	if len(interceptCA) > 0 {
		f, err := os.OpenFile(ca, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.Write(interceptCA)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	env := append(cleanCLIEnv(t.TempDir()), "GH_TOKEN=placeholder-not-a-real-token", "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_NO_EXTENSION_UPDATE_NOTIFIER=1", "GH_TELEMETRY=0", "HTTPS_PROXY="+endpoint, "SSL_CERT_FILE="+ca)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	repoDir := t.TempDir()
	gitConfig := filepath.Join(t.TempDir(), "config")
	env = append(env, "GIT_CONFIG_GLOBAL="+gitConfig)
	for _, args := range [][]string{
		{"init", "-b", "main", repoDir},
		{"-C", repoDir, "remote", "add", "origin", "https://github.com/" + owner + "/repo"},
		{"config", "--file", gitConfig, "http.https://github.com/.proxy", endpoint},
		{"config", "--file", gitConfig, "http.sslCAInfo", ca},
		{"config", "--file", gitConfig, "http.proxySSLCAInfo", ca},
		{"config", "--file", gitConfig, "--add", "url.https://github.com/.insteadOf", "git@github.com:"},
		{"config", "--file", gitConfig, "--add", "url.https://github.com/.insteadOf", "ssh://git@github.com/"},
	} {
		cmd := exec.Command(git, args...)
		cmd.Env = env
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("git setup: %v\n%s", e, b)
		}
	}
	for _, tt := range []struct {
		args  []string
		allow bool
	}{
		{[]string{"api", "user"}, true},
		{[]string{"repo", "view", "--json", "nameWithOwner"}, true},
		{[]string{"issue", "create", "--title", "Proxy test", "--body", "Testing restricted access"}, true},
		{[]string{"api", "--method", "POST", "repos/" + owner + "/repo/issues", "-f", "title=hello"}, true},
		{[]string{"api", "--method", "PATCH", "repos/" + owner + "/repo/issues/2", "-f", "body=edit"}, false},
		{[]string{"api", "graphql", "-f", `query=mutation {closeIssue(input:{issueId:"issue"}){clientMutationId}}`}, true},
		{[]string{"api", "graphql", "-f", `query=mutation {deleteRef(input:{refId:"main-ref"}){clientMutationId}}`}, false},
	} {
		cmd := exec.Command(gh, tt.args...)
		cmd.Env = env
		cmd.Dir = repoDir
		b, err := cmd.CombinedOutput()
		if (err == nil) != tt.allow {
			t.Fatalf("gh %v: %v\n%s", tt.args, err, b)
		}
	}
	for _, remote := range []string{"git@github.com:" + owner + "/repo", "ssh://git@github.com/" + owner + "/repo"} {
		cmd := exec.Command(git, "remote", "set-url", "origin", remote)
		cmd.Dir = repoDir
		cmd.Env = env
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("set remote: %v\n%s", e, b)
		}
		cmd = exec.Command(gh, "repo", "view", "--json", "nameWithOwner")
		cmd.Dir = repoDir
		cmd.Env = env
		if b, e := cmd.CombinedOutput(); e != nil || !strings.Contains(string(b), owner+"/repo") {
			t.Fatalf("gh repo detection for %s: %v\n%s", remote, e, b)
		}
	}
}

func TestInterceptedCLIs(t *testing.T) {
	t.Run("git", func(t *testing.T) { testGitCLI(t, "acme", "Organization", true) })
	t.Run("gh", func(t *testing.T) { testGHCLI(t, "acme", "Organization", true) })
}
