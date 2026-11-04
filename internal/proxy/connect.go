package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"time"
)

// CONNECT authority selects routing; SNI and HTTP authority must agree with it.
func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := authority(r.Host, "")
	if err != nil {
		http.Error(w, "invalid CONNECT target", http.StatusBadRequest)
		return
	}
	if err := p.checkHost(host); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if ip := net.ParseIP(host); ip != nil {
		// Literal IP blocks are rejected before acknowledging CONNECT.
		addr, _ := netip.ParseAddr(host)
		if err := p.checkIP(addr, port); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}
	github := host == "api.github.com" || host == "github.com"
	intercept := github || p.config.interceptAll()
	if github && (port != "443" || p.tlsConfig == nil) || p.config.interceptAll() && p.ca == nil {
		http.Error(w, "CONNECT target denied", http.StatusForbidden)
		return
	}
	h, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "HTTP/1.1 is required", http.StatusBadRequest)
		return
	}
	var upstream net.Conn
	if !intercept {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		upstream, err = p.checkedDial(ctx, "tcp", net.JoinHostPort(host, port))
		cancel()
		if err != nil {
			upstreamError(w, r, err)
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
	if !intercept {
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
	innerTLS := p.tlsConfig.Clone()
	if p.config.interceptAll() {
		innerTLS = &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			sni, e := normalizeHost(hello.ServerName)
			if hello.ServerName == "" && net.ParseIP(host) != nil {
				sni, e = host, nil
			}
			if e != nil || sni != host {
				return nil, fmt.Errorf("TLS SNI differs from CONNECT host")
			}
			return p.ca.certificate(host)
		}}
	}
	// Return a concrete *tls.Conn to net/http for ALPN/HTTP2. The underlying
	// close notification keeps this CONNECT handler alive through upgrades.
	conn := &closingConn{Conn: buffered, done: make(chan struct{})}
	tc := tls.Server(conn, innerTLS)
	if tc.HandshakeContext(r.Context()) != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	l := &oneListener{conn: conn, acceptedConn: tc}
	s := &http.Server{TLSConfig: innerTLS, Handler: http.HandlerFunc(func(w http.ResponseWriter, inner *http.Request) {
		innerHost, innerPort, err := authority(inner.Host, "443")
		if err != nil || inner.Method == http.MethodConnect || innerHost != host || innerPort != port || inner.URL.IsAbs() || inner.URL.User != nil {
			http.Error(w, "tunnel host mismatch", http.StatusForbidden)
			return
		}
		if github {
			inner.Host = host
			p.ServeHTTP(w, inner)
			return
		}
		p.forwardHTTPS(w, inner, net.JoinHostPort(host, port))
	}), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute}
	_ = s.Serve(l)
}

func upstreamError(w http.ResponseWriter, _ *http.Request, err error) {
	if errors.Is(err, errDestinationDenied) {
		http.Error(w, "destination blocked by policy", http.StatusForbidden)
	} else {
		// Do not log URL/query, credentials or potentially sensitive TLS errors.
		log.Print("upstream connection failed")
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
}

func (p *Proxy) forwardHTTPS(w http.ResponseWriter, r *http.Request, target string) {
	rp := &httputil.ReverseProxy{Transport: p.transport, FlushInterval: -1, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(&url.URL{Scheme: "https", Host: target})
		pr.Out.Host = pr.In.Host
		pr.Out.Header.Del("Proxy-Authorization")
		pr.Out.Header.Del("Proxy-Connection")
	}, ErrorHandler: upstreamError}
	rp.ServeHTTP(w, r)
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
