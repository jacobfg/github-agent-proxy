package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func enableInterception(t *testing.T, p *Proxy) []byte {
	t.Helper()
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "key.pem")
	if err := GenerateCA(cert, key); err != nil {
		t.Fatal(err)
	}
	ca, err := loadCA(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	p.ca = ca
	p.config.Policy = policyForTest(t, "", "")
	b, err := os.ReadFile(cert)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func interceptionClient(t *testing.T, upstream *httptest.Server, trustOrigin bool) (*Proxy, *http.Client, string) {
	t.Helper()
	p, _ := fixture(t)
	ca := enableInterception(t, p)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ForceAttemptHTTP2 = true
	roots := x509.NewCertPool()
	if trustOrigin {
		roots.AddCert(upstream.Certificate())
	}
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	transport.DialContext = p.checkedDial
	p.transport = transport
	endpoint, outerCA := cliServer(t, p)
	clientRoots := x509.NewCertPool()
	clientRoots.AppendCertsFromPEM(ca)
	b, _ := os.ReadFile(outerCA)
	clientRoots.AppendCertsFromPEM(b)
	proxyURL, _ := url.Parse(endpoint)
	ct := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: clientRoots, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true, ProxyConnectHeader: http.Header{"Proxy-Authorization": []string{"Basic outer-secret"}}}
	t.Cleanup(ct.CloseIdleConnections)
	return p, &http.Client{Transport: ct, Timeout: 5 * time.Second}, endpoint
}

func TestInterceptHTTP2StreamingAndCredentials(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Error("upstream did not use HTTP/2")
		}
		if r.Header.Get("Authorization") != "Bearer service-token" || r.Header.Get("Cookie") != "session=client" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("credentials changed or leaked")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	_, client, _ := interceptionClient(t, upstream, true)
	req, _ := http.NewRequest("POST", upstream.URL+"/stream", strings.NewReader("request"))
	req.Header.Set("Authorization", "Bearer service-token")
	req.Header.Set("Cookie", "session=client")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.ProtoMajor != 2 || res.TLS.Version < tls.VersionTLS12 {
		t.Fatalf("inner protocol: %s", res.Proto)
	}
	if bytes.Equal(res.TLS.PeerCertificates[0].Raw, upstream.Certificate().Raw) {
		t.Fatal("origin was tunnelled")
	}
	b := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(res.Body, b); err != nil || string(b) != "data: first\n\n" {
		t.Fatalf("stream blocked: %q %v", b, err)
	}
}

func TestInterceptUpstreamTLSVersions(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS10, tls.VersionTLS11, tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(tls.VersionName(version), func(t *testing.T) {
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
			upstream.TLS = &tls.Config{MinVersion: version, MaxVersion: version}
			upstream.StartTLS()
			defer upstream.Close()
			_, client, _ := interceptionClient(t, upstream, true)
			res, err := client.Get(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			want := 200
			if version < tls.VersionTLS12 {
				want = 502
			}
			if res.StatusCode != want {
				t.Fatalf("status=%d want=%d", res.StatusCode, want)
			}
		})
	}
}

func TestInterceptRejectsUntrustedOrigin(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted origin reached") }))
	defer upstream.Close()
	_, client, _ := interceptionClient(t, upstream, false)
	res, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 502 {
		t.Fatal(res.Status)
	}
}

func TestInterceptClientTLSAndAuthority(t *testing.T) {
	p, _ := fixture(t)
	enableInterception(t, p)
	endpoint, _ := cliServer(t, p)
	for _, version := range []uint16{tls.VersionTLS10, tls.VersionTLS11, tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(tls.VersionName(version), func(t *testing.T) {
			u, _ := url.Parse(endpoint)
			c, err := tls.Dial("tcp", u.Host, &tls.Config{InsecureSkipVerify: true, MinVersion: version, MaxVersion: version})
			if c != nil {
				c.Close()
			}
			if (err == nil) != (version >= tls.VersionTLS12) {
				t.Fatalf("outer TLS: %v", err)
			}
			inner := tls.Client(connectRaw(t, endpoint, "example.com:443"), &tls.Config{InsecureSkipVerify: true, ServerName: "example.com", MinVersion: version, MaxVersion: version})
			err = inner.Handshake()
			inner.Close()
			if (err == nil) != (version >= tls.VersionTLS12) {
				t.Fatalf("inner TLS: %v", err)
			}
		})
	}
	for _, sni := range []string{"", "different.example"} {
		inner := tls.Client(connectRaw(t, endpoint, "example.com:443"), &tls.Config{InsecureSkipVerify: true, ServerName: sni})
		if err := inner.Handshake(); err == nil {
			t.Error("accepted mismatched/absent SNI")
		}
		inner.Close()
	}
	for _, host := range []string{"other.example", "example.com:8445"} {
		inner := tls.Client(connectRaw(t, endpoint, "example.com:443"), &tls.Config{InsecureSkipVerify: true, ServerName: "example.com"})
		fmt.Fprintf(inner, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host)
		res, err := http.ReadResponse(bufio.NewReader(inner), nil)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		inner.Close()
		if res.StatusCode != 403 {
			t.Fatal("authority accepted", res.Status)
		}
	}
	raw := connectRaw(t, endpoint, "example.com:443")
	fmt.Fprint(raw, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	b := make([]byte, 1024)
	n, _ := raw.Read(b)
	raw.Close()
	if strings.Contains(string(b[:n]), "200") {
		t.Fatal("plaintext tunnel accepted")
	}
}

func TestInterceptWebSocketUpgradeAndShutdown(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			t.Error("upgrade stripped")
		}
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		rw.Flush()
		// The proxy transports frames opaquely after upgrade.
		io.Copy(c, rw)
	}))
	defer upstream.Close()
	p, _, endpoint := interceptionClient(t, upstream, true)
	u, _ := url.Parse(upstream.URL)
	inner := tls.Client(connectRaw(t, endpoint, u.Host), &tls.Config{InsecureSkipVerify: true})
	defer inner.Close()
	fmt.Fprintf(inner, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", u.Host)
	reader := bufio.NewReader(inner)
	res, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 101 {
		t.Fatal(res.Status)
	}
	frame := []byte{0x81, 0x82, 1, 2, 3, 4, 'h' ^ 1, 'i' ^ 2}
	if _, err := inner.Write(frame); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(frame))
	if _, err := io.ReadFull(reader, got); err != nil || !bytes.Equal(got, frame) {
		t.Fatalf("upgrade: %x %v", got, err)
	}
	p.Close()
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("upgraded connection survived shutdown")
	}
}

func TestCertificateCacheAndValidation(t *testing.T) {
	p, _ := fixture(t)
	enableInterception(t, p)
	for _, host := range []string{"example.com", "192.0.2.1", "2001:db8::1"} {
		c, err := p.ca.certificate(host)
		if err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		roots.AddCert(p.ca.cert)
		if _, err := c.Leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots}); err != nil {
			t.Fatal(err)
		}
		if err := c.Leaf.VerifyHostname("wrong.example"); err == nil {
			t.Fatal("wrong SAN accepted")
		}
		again, _ := p.ca.certificate(host)
		if c != again {
			t.Fatal("cache miss")
		}
	}
	old, _ := p.ca.certificate("example.com")
	now := time.Now().Add(25 * time.Hour)
	p.ca.now = func() time.Time { return now }
	renewed, err := p.ca.certificate("example.com")
	if err != nil || renewed == old {
		t.Fatal("expired leaf reused", err)
	}
	for i := 0; i < certificateCacheLimit+1; i++ {
		if _, err := p.ca.certificate(fmt.Sprintf("host%d.example", i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.ca.cache) > certificateCacheLimit {
		t.Fatal("unbounded cache")
	}
	p.ca.now = func() time.Time { return p.ca.cert.NotAfter.Add(time.Second) }
	if _, err := p.ca.certificate("example.com"); err == nil {
		t.Fatal("expired CA accepted")
	}
}

func TestInterceptDNSBlock(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("blocked origin reached") }))
	defer upstream.Close()
	p, client, _ := interceptionClient(t, upstream, true)
	p.config.Policy = policyForTest(t, `{"deny_ips":["192.0.2.1"]}`, "")
	p.lookupIP = testLookup
	p.dialContext = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("blocked IP dialled")
		return nil, nil
	}
	res, err := client.Get("https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal(res.Status)
	}
}

func TestInterceptRejectsWrongOriginName(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("wrong-name origin reached") }))
	defer upstream.Close()
	p, client, _ := interceptionClient(t, upstream, true)
	p.lookupIP = testLookup
	p.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	res, err := client.Get("https://wrong.example/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 502 {
		t.Fatal(res.Status)
	}
}

func TestInterceptionStatus(t *testing.T) {
	p, _ := fixture(t)
	enableInterception(t, p)
	p.config.CACertFile = "/private/ca.pem"
	p.config.CAKeyFile = "/private/key.pem"
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/status", nil)
	r.Host = "localhost"
	p.ServeHTTP(w, r)
	body := w.Body.String()
	for _, want := range []string{`"non_github_https":"intercept"`, `"all_https_tls_enforced":true`, p.ca.fingerprint, `"policy":`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing status %s", want)
		}
	}
	for _, secret := range []string{p.config.Token, p.config.CACertFile, p.config.CAKeyFile} {
		if strings.Contains(body, secret) {
			t.Fatal("status exposed secret/path")
		}
	}
}
