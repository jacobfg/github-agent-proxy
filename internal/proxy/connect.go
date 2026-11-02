package proxy

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CONNECT preserves the destination URL. Only the two authenticated GitHub
// origins terminate TLS here; other destinations retain end-to-end TLS.
func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 || host == "" || strings.ContainsAny(host, "/\\@?#% \t\r\n") {
		http.Error(w, "invalid CONNECT target", http.StatusBadRequest)
		return
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	github := host == "api.github.com" || host == "github.com"
	if github && (n != 443 || p.tlsConfig == nil) {
		http.Error(w, "CONNECT target denied", http.StatusForbidden)
		return
	}
	h, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "HTTP/1.1 is required", http.StatusBadRequest)
		return
	}
	var upstream net.Conn
	if !github {
		dial := p.dialContext
		if dial == nil {
			dial = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		upstream, err = dial(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(n)))
		cancel()
		if err != nil {
			http.Error(w, "CONNECT upstream unavailable", http.StatusBadGateway)
			return
		}
		if !p.track(upstream) {
			http.Error(w, "proxy is shutting down", http.StatusServiceUnavailable)
			return
		}
		defer p.untrack(upstream)
	}
	c, rw, err := h.Hijack()
	if err != nil {
		return
	}
	if !p.track(c) {
		return
	}
	defer p.untrack(c)
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if rw.Flush() != nil {
		return
	}
	buffered := &bufferedConn{Conn: c, reader: rw.Reader}
	if !github {
		_ = c.SetDeadline(time.Time{})
		// Copy only tunnel bytes: outer headers (including Proxy-Authorization)
		// and the proxy's PAT never reach an arbitrary destination.
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(upstream, buffered)
			if half, ok := upstream.(interface{ CloseWrite() error }); ok {
				_ = half.CloseWrite()
			} else {
				_ = upstream.Close()
			}
			close(done)
		}()
		_, _ = io.Copy(c, upstream)
		_ = c.Close()
		_ = upstream.Close()
		<-done
		return
	}
	tc := tls.Server(buffered, p.tlsConfig)
	if tc.HandshakeContext(r.Context()) != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	conn := &closingConn{Conn: tc, done: make(chan struct{})}
	l := &oneListener{conn: conn}
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, inner *http.Request) {
		innerHost := strings.ToLower(inner.Host)
		if h, port, err := net.SplitHostPort(innerHost); err == nil {
			if port != "443" {
				http.Error(w, "tunnel host mismatch", http.StatusForbidden)
				return
			}
			innerHost = h
		}
		if inner.Method == http.MethodConnect || strings.TrimSuffix(innerHost, ".") != host {
			http.Error(w, "tunnel host mismatch", http.StatusForbidden)
			return
		}
		inner.Host = host
		p.ServeHTTP(w, inner)
	}), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute}
	_ = s.Serve(l)
}

func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = c.Close()
		return false
	}
	p.tunnels[c] = true
	return true
}

func (p *Proxy) untrack(c net.Conn) {
	_ = c.Close()
	p.mu.Lock()
	delete(p.tunnels, c)
	p.mu.Unlock()
}
