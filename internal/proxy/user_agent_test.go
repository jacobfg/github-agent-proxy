package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// Observe headers at a real origin: checking a mocked RoundTripper would miss
// the default User-Agent inserted by Go's HTTP/1 or HTTP/2 transport.
func TestUserAgentForwarding(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("HTTP2=%v", http2), func(t *testing.T) {
			type received struct {
				value   string
				present bool
				proto   int
			}
			requests := make(chan received, 1)
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, present := r.Header["User-Agent"]
				requests <- received{r.UserAgent(), present, r.ProtoMajor}
				io.WriteString(w, `{"login":"bot"}`)
			}))
			upstream.EnableHTTP2 = http2
			upstream.StartTLS()
			defer upstream.Close()
			target, _ := url.Parse(upstream.URL)
			transport := upstream.Client().Transport.(*http.Transport).Clone()
			transport.Proxy = nil
			transport.ForceAttemptHTTP2 = http2
			defer transport.CloseIdleConnections()
			p, _ := fixture(t)
			p.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				out := r.Clone(r.Context())
				out.URL.Scheme, out.URL.Host = target.Scheme, target.Host
				return transport.RoundTrip(out)
			})
			p.client = &http.Client{Transport: p.transport}
			check := func(want string) {
				t.Helper()
				got := <-requests
				proto := 1
				if http2 {
					proto = 2
				}
				if got.value != want || got.present != (want != "") || got.proto != proto {
					t.Fatalf("origin received %+v; want User-Agent %q over HTTP/%d", got, want, proto)
				}
			}
			for _, host := range []string{"api.github.com", "github.com", "service.example"} {
				for _, agent := range []string{"git/2.50.0", "GitHub CLI test", "", "absent"} {
					t.Run(host+"/"+agent, func(t *testing.T) {
						r := httptest.NewRequest("GET", "/user", nil)
						r.Host = host
						want := agent
						if agent == "absent" {
							want = ""
						} else {
							r.Header.Set("User-Agent", agent)
						}
						w := httptest.NewRecorder()
						if host == "service.example" {
							p.forwardHTTPS(w, r, host+":443")
						} else {
							p.ServeHTTP(w, r)
						}
						if w.Code != http.StatusOK {
							t.Fatalf("forwarding failed: %d", w.Code)
						}
						check(want)
					})
				}
			}
			for _, path := range []string{"/user", "/repos/acme/repo", "/repos/acme/repo/pulls/1"} {
				var result map[string]any
				if err := p.api(context.Background(), "GET", path, nil, &result); err != nil {
					t.Fatal(err)
				}
				check("github-agent-proxy")
			}
		})
	}
}
