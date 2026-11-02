package proxy

import (
	"encoding/json"
	"net/http"
	"sort"
)

// serveLocal handles diagnostics on the local endpoint only. Build responses
// explicitly: Config contains the PAT and private filesystem paths.
func (p *Proxy) serveLocal(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/status" && r.URL.Path != "/healthz" {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	code := http.StatusOK
	var body any
	if r.URL.Path == "/healthz" {
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		status := "ok"
		if closed {
			code = http.StatusServiceUnavailable
			status = "shutting_down"
		}
		body = struct {
			Status string `json:"status"`
		}{status}
	} else {
		body = struct {
			AllowedOrgs    []string `json:"allowed_orgs"`
			AllowedUsers   []string `json:"allowed_users"`
			BranchPrefix   string   `json:"branch_prefix"`
			TokenOwner     string   `json:"token_owner"`
			ListenAddr     string   `json:"listen_addr"`
			MaxPushBytes   int64    `json:"max_push_bytes"`
			GitHubReads    string   `json:"github_reads"`
			GitHubWrites   string   `json:"github_writes"`
			NonGitHubHTTPS string   `json:"non_github_https"`
		}{
			AllowedOrgs:    enabledLogins(p.config.Organizations),
			AllowedUsers:   enabledLogins(p.config.Users),
			BranchPrefix:   p.config.BranchPrefix,
			TokenOwner:     p.login,
			ListenAddr:     p.config.Listen,
			MaxPushBytes:   p.config.MaxPushBytes,
			GitHubReads:    "unfiltered",
			GitHubWrites:   "default-deny",
			NonGitHubHTTPS: "tunnel",
		}
	}
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(body)
	}
	return true
}

func enabledLogins(logins map[string]bool) []string {
	result := make([]string, 0, len(logins))
	for login, enabled := range logins {
		if enabled {
			result = append(result, login)
		}
	}
	sort.Strings(result)
	return result
}
