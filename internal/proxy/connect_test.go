package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestConnectForwardHTTPS(t *testing.T) {
	p, _ := fixture(t)
	endpoint, _ := cliServer(t, p)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer client-service-token" || r.Header.Get("Cookie") != "session=client" {
			t.Error("non-GitHub credentials were modified")
		}
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("outer proxy credentials leaked upstream")
		}
		if r.ProtoMajor != 2 {
			t.Error("end-to-end HTTP/2 was not preserved")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.Copy(w, r.Body)
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	roots := x509.NewCertPool()
	// Independent origin certificate: the proxy must not replace it.
	roots.AddCert(upstream.Certificate())
	proxyURL, _ := url.Parse(endpoint)
	proxyCert, _ := x509.ParseCertificate(p.tlsConfig.Certificates[0].Certificate[0])
	roots.AddCert(proxyCert)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots}, ForceAttemptHTTP2: true, ProxyConnectHeader: http.Header{"Proxy-Authorization": {"Basic outer-secret"}}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	req, _ := http.NewRequest("POST", upstream.URL+"/stream", strings.NewReader("data: second\n\n"))
	req.Header.Set("Authorization", "Bearer client-service-token")
	req.Header.Set("Cookie", "session=client")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if !bytes.Equal(res.TLS.PeerCertificates[0].Raw, upstream.Certificate().Raw) {
		t.Fatal("origin certificate replaced")
	}
	body, err := io.ReadAll(res.Body)
	if err != nil || string(body) != "data: first\n\ndata: second\n\n" {
		t.Fatalf("stream: %q %v", body, err)
	}
}

func TestConnectRejectsInvalidTargets(t *testing.T) {
	for _, target := range []string{"example.com", ":443", "example.com:0", "example.com:65536", "example.com:https", "user@example.com:443", "github.com:8443", "API.GITHUB.COM.:8443"} {
		t.Run(target, func(t *testing.T) {
			p, _ := fixture(t)
			r := httptest.NewRequest("CONNECT", "http://localhost", nil)
			r.Host = target
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != 400 && w.Code != 403 {
				t.Fatalf("target accepted: %d", w.Code)
			}
		})
	}
}

func TestConnectNormalizesGitHub(t *testing.T) {
	p, _ := fixture(t)
	endpoint, _ := cliServer(t, p)
	p.dialContext = func(context.Context, string, string) (net.Conn, error) {
		t.Error("GitHub escaped policy interception")
		return nil, errors.New("unexpected relay")
	}
	for _, host := range []string{"API.GITHUB.COM", "api.github.com.", "GITHUB.COM."} {
		c := connectRaw(t, endpoint, host+":443")
		// This test inspects interception/routing, not certificate validation.
		inner := tls.Client(c, &tls.Config{InsecureSkipVerify: true})
		fmt.Fprintf(inner, "DELETE /repos/acme/repo HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host)
		res, err := http.ReadResponse(bufio.NewReader(inner), nil)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		inner.Close()
		if res.StatusCode != 403 {
			t.Fatalf("write escaped policy: %d", res.StatusCode)
		}
	}
}

func TestConnectDialFailure(t *testing.T) {
	p, _ := fixture(t)
	endpoint, _ := cliServer(t, p)
	p.dialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("private dial error")
	}
	u, _ := url.Parse(endpoint)
	c, err := tls.Dial("tcp", u.Host, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 502 {
		t.Fatal(res.Status)
	}
}

func TestConnectBufferedBytesAndShutdown(t *testing.T) {
	p, _ := fixture(t)
	endpoint, _ := cliServer(t, p)
	server, relay := net.Pipe()
	defer server.Close()
	p.dialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" || addr != "example.com:9443" {
			t.Errorf("dial %s %s", network, addr)
		}
		return relay, nil
	}
	u, _ := url.Parse(endpoint)
	c, err := tls.Dial("tcp", u.Host, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	// Pipeline bytes with CONNECT to exercise net/http's buffered reader.
	fmt.Fprint(c, "CONNECT example.com:9443 HTTP/1.1\r\nHost: example.com:9443\r\n\r\nearly bytes")
	reader := bufio.NewReader(c)
	res, err := http.ReadResponse(reader, nil)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", res, err)
	}
	server.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, len("early bytes"))
	if _, err := io.ReadFull(server, b); err != nil || string(b) != "early bytes" {
		t.Fatalf("buffer: %q %v", b, err)
	}
	p.Close()
	if _, err := server.Read(b); err == nil {
		t.Fatal("upstream remained open after shutdown")
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("client remained open after shutdown")
	}
}

func TestConnectHalfClose(t *testing.T) {
	p, _ := fixture(t)
	endpoint, _ := cliServer(t, p)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		b, err := io.ReadAll(c)
		if err == nil {
			_, err = c.Write(append([]byte("reply:"), b...))
		}
		done <- err
	}()
	c := connectRaw(t, endpoint, listener.Addr().String()).(*bufferedConn)
	if _, err := c.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := c.Conn.(*tls.Conn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(c)
	if err != nil || string(b) != "reply:request" {
		t.Fatalf("reply after half-close: %q %v", b, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func connectRaw(t *testing.T, endpoint, target string) net.Conn {
	t.Helper()
	u, _ := url.Parse(endpoint)
	c, err := tls.Dial("tcp", u.Host, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	r := bufio.NewReader(c)
	res, err := http.ReadResponse(r, nil)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", res, err)
	}
	return &bufferedConn{Conn: c, reader: r}
}
