package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxJSON = 2 << 20

var errDenied = errors.New("write is outside the configured allowlist")

type Proxy struct {
	config      Config
	login       string
	client      *http.Client
	transport   http.RoundTripper
	tlsConfig   *tls.Config
	mu          sync.Mutex
	tunnels     map[net.Conn]bool
	closed      bool
	dialContext func(context.Context, string, string) (net.Conn, error)
}

// New resolves the PAT identity before accepting traffic. Upstreams are fixed;
// neither a client header nor an environment proxy can redirect the credential.
func New(ctx context.Context, c Config, tlsConfig *tls.Config) (*Proxy, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.ResponseHeaderTimeout = 30 * time.Second
	p := &Proxy{config: c, transport: t, tlsConfig: tlsConfig, tunnels: map[net.Conn]bool{}}
	p.client = &http.Client{Transport: t, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var user struct {
		Login string `json:"login"`
	}
	if err := p.api(ctx, "GET", "/user", nil, &user); err != nil || user.Login == "" {
		return nil, fmt.Errorf("cannot resolve PAT identity (check token and connectivity)")
	}
	p.login = user.Login
	return p, nil
}

func (p *Proxy) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for c := range p.tunnels {
		_ = c.Close()
	}
	if t, ok := p.transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

func (p *Proxy) api(ctx context.Context, method, path string, body any, result any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, reader)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+p.config.Token)
	r.Header.Set("Accept", "application/vnd.github+json")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "github-agent-proxy")
	res, err := p.client.Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("GitHub lookup returned %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxJSON+1))
	if err != nil || len(b) > maxJSON {
		return fmt.Errorf("invalid GitHub lookup response")
	}
	return json.Unmarshal(b, result)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if r.URL.IsAbs() || r.URL.User != nil {
		http.Error(w, "absolute URLs are not accepted", 400)
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	path := r.URL.Path
	origin := "api.github.com"
	switch host {
	case "api.github.com":
	case "github.com":
		origin = "github.com"
	case "localhost", "127.0.0.1", "::1":
		if p.serveLocal(w, r) {
			return
		}
		if strings.HasPrefix(path, "/git/") {
			origin = "github.com"
			path = strings.TrimPrefix(path, "/git")
		} else if strings.HasPrefix(path, "/api/") {
			path = strings.TrimPrefix(path, "/api")
		} else {
			http.Error(w, "use /api/, /git/, /status, or /healthz", 404)
			return
		}
	default:
		http.Error(w, "host is not allowed", 403)
		return
	}
	// Eliminate alternate interpretations between the policy and GitHub routing.
	if strings.ContainsAny(path, "\\%\x00\r\n") || strings.Contains(path, "//") {
		http.Error(w, "ambiguous path", 400)
		return
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			http.Error(w, "ambiguous path", 400)
			return
		}
	}
	if r.Method != "GET" && r.Method != "HEAD" && !(origin == "github.com" && strings.HasSuffix(path, "/git-upload-pack")) && r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity" {
		http.Error(w, "compressed requests are not supported", 415)
		return
	}
	var err error
	if origin == "api.github.com" {
		err = p.authorizeAPI(r, path)
	} else {
		err = p.authorizeGit(w, r, path)
	}
	if err != nil {
		http.Error(w, "denied: "+err.Error(), http.StatusForbidden)
		return
	}
	target := &url.URL{Scheme: "https", Host: origin}
	rp := &httputil.ReverseProxy{Transport: p.transport, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		pr.Out.URL.Path = path
		pr.Out.URL.RawPath = ""
		pr.Out.Host = origin
		pr.Out.Header = make(http.Header)
		for _, name := range []string{"Accept", "Content-Type", "Content-Encoding", "Git-Protocol", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "Range", "X-Github-Api-Version"} {
			for _, v := range pr.In.Header.Values(name) {
				pr.Out.Header.Add(name, v)
			}
		}
		pr.Out.Header.Set("Authorization", "Bearer "+p.config.Token)
		if origin == "github.com" {
			pr.Out.SetBasicAuth("x-access-token", p.config.Token)
		}
		pr.Out.Header.Set("User-Agent", "github-agent-proxy")
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "GitHub upstream unavailable", 502)
	}}
	rp.ServeHTTP(w, r)
}

type bufferedConn struct {
	net.Conn
	reader io.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

type closingConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *closingConn) Close() error { c.once.Do(func() { close(c.done) }); return c.Conn.Close() }

type oneListener struct {
	conn     *closingConn
	accepted bool
}

func (l *oneListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.conn, nil
	}
	<-l.conn.done
	return nil, net.ErrClosed
}
func (l *oneListener) Close() error   { return l.conn.Close() }
func (l *oneListener) Addr() net.Addr { return l.conn.LocalAddr() }
