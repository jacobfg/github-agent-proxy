package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestLocalStatus(t *testing.T) {
	p, _ := fixture(t)
	p.config.Organizations = map[string]bool{"zendesk": true, "acme": true, "disabled": false}
	p.config.Users = nil
	p.config.Listen = "127.0.0.1:8443"
	p.config.CertFile = "/private/server.pem"
	p.config.KeyFile = "/private/server-key.pem"
	p.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("local diagnostics contacted upstream")
		return nil, nil
	})
	// Diagnostics reflect the loaded configuration, not later env changes.
	t.Setenv("POLICY_FILE", "/nonexistent/later-policy.json")
	for _, host := range []string{"localhost", "127.0.0.1:8443", "[::1]:8443"} {
		r := httptest.NewRequest("GET", "/status", nil)
		r.Host = host
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("status response: %d %v", w.Code, w.Header())
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"allowed_orgs": []any{"acme", "zendesk"}, "allowed_users": []any{},
			"branch_prefix": "agent/", "token_owner": "bot", "listen_addr": "127.0.0.1:8443",
			"max_push_bytes": float64(1 << 20), "github_reads": "unfiltered",
			"github_writes": "default-deny", "non_github_https": "tunnel",
			"tls_min_version": "1.2", "all_https_tls_enforced": false,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("status = %#v; want %#v", got, want)
		}
		for _, secret := range []string{p.config.Token, p.config.CertFile, p.config.KeyFile} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("status exposed sensitive configuration")
			}
		}
	}
}

func TestLocalHealthAndMethods(t *testing.T) {
	p, _ := fixture(t)
	p.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("local diagnostics contacted upstream")
		return nil, nil
	})
	for _, path := range []string{"/status", "/healthz"} {
		for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
			r := httptest.NewRequest(method, path, nil)
			r.Host = "localhost"
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			switch method {
			case "GET", "HEAD":
				if w.Code != 200 {
					t.Fatalf("%s %s: %d", method, path, w.Code)
				}
				if method == "HEAD" && w.Body.Len() != 0 {
					t.Fatal("HEAD returned a body")
				}
				if method == "GET" && path == "/healthz" && w.Body.String() != "{\"status\":\"ok\"}\n" {
					t.Fatalf("unexpected health: %s", w.Body)
				}
			default:
				if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
					t.Fatalf("%s %s: %d", method, path, w.Code)
				}
			}
		}
	}
	p.Close()
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.Host = "localhost"
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 503 || w.Body.String() != "{\"status\":\"shutting_down\"}\n" {
		t.Fatalf("shutdown health: %d %s", w.Code, w.Body)
	}
}

func TestDiagnosticsDoNotShadowGitHub(t *testing.T) {
	p, _ := fixture(t)
	calls := 0
	p.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.github.com" && r.URL.Host != "github.com" {
			t.Fatal("unexpected upstream")
		}
		return response(map[string]string{"source": "upstream"}), nil
	})
	for _, tt := range []struct{ host, path string }{
		{"api.github.com", "/status"}, {"api.github.com", "/healthz"},
		{"github.com", "/status"}, {"github.com", "/healthz"},
		{"localhost", "/api/status"}, {"localhost", "/api/healthz"},
	} {
		r := httptest.NewRequest("GET", tt.path, nil)
		r.Host = tt.host
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "upstream") {
			t.Fatalf("intercepted upstream request: %s%s: %d %s", tt.host, tt.path, w.Code, w.Body)
		}
	}
	if calls != 6 {
		t.Fatalf("expected 6 upstream requests, got %d", calls)
	}
}
