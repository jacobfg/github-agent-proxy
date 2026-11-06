package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func configEnvironment(t *testing.T, body string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"POLICY_FILE": file, "GITHUB_TOKEN": "secret", "GITHUB_TOKEN_FILE": "",
		"TLS_CERT_FILE": "cert.pem", "TLS_KEY_FILE": "key.pem", "LISTEN_ADDR": "",
		"ALLOW_CONTAINER_BIND": "", "MAX_PUSH_BYTES": "",
		"INTERCEPT_CA_CERT_FILE": "", "INTERCEPT_CA_KEY_FILE": "",
	} {
		t.Setenv(k, v)
	}
	return file
}

func TestConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name, key, value string
		valid            bool
	}{
		{"defaults", "LISTEN_ADDR", "", true},
		{"missing policy", "POLICY_FILE", "", false},
		{"blank policy", "POLICY_FILE", " \t", false},
		{"missing token", "GITHUB_TOKEN", "", false},
		{"missing TLS", "TLS_KEY_FILE", "", false},
		{"public bind", "LISTEN_ADDR", "0.0.0.0:8443", false},
		{"IPv6 loopback", "LISTEN_ADDR", "[::1]:8443", true},
		{"invalid size", "MAX_PUSH_BYTES", "0", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			configEnvironment(t, DefaultPolicy())
			t.Setenv(tt.key, tt.value)
			c, err := FromEnv()
			if (err == nil) != tt.valid {
				t.Fatalf("error=%v", err)
			}
			if tt.key == "POLICY_FILE" && (err == nil || !strings.Contains(err.Error(), "POLICY_FILE is required")) {
				t.Fatalf("missing policy diagnostic: %v", err)
			}
			if err == nil && c.Policy == nil {
				t.Fatal("configuration has no policy")
			}
		})
	}
}

func TestPolicyFileFailures(t *testing.T) {
	for _, body := range []string{"", "{", "{}", `{"https_mode":"tunnel","github":{"branch_prefix":""}}`, `{"https_mode":"tunnel","github":{"branch_prefix":"agent"}}`, `{"https_mode":"tunnel","github":{"branch_prefix":"agent/../"}}`} {
		t.Run(body, func(t *testing.T) {
			configEnvironment(t, body)
			if _, err := FromEnv(); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		file := configEnvironment(t, DefaultPolicy())
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "read policy") {
			t.Fatalf("missing policy accepted: %v", err)
		}
	})
}

func TestOwnerAllowlistConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name        string
		orgs, users []string
	}{
		{"orgs only", []string{"Acme", "OTHER"}, []string{}},
		{"users only", []string{}, []string{"JacobFG", "Bot"}},
		{"both", []string{"Acme"}, []string{"JacobFG"}},
		{"neither", []string{}, []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"https_mode": "tunnel",
				"github":     map[string]any{"allowed_orgs": tt.orgs, "allowed_users": tt.users, "branch_prefix": "work/"},
			})
			if err != nil {
				t.Fatal(err)
			}
			configEnvironment(t, string(body))
			c, err := FromEnv()
			if err != nil {
				t.Fatal(err)
			}
			wantOrgs, wantUsers := map[string]bool{}, map[string]bool{}
			for _, owner := range tt.orgs {
				wantOrgs[strings.ToLower(owner)] = true
			}
			for _, owner := range tt.users {
				wantUsers[strings.ToLower(owner)] = true
			}
			if !reflect.DeepEqual(c.Organizations, wantOrgs) || !reflect.DeepEqual(c.Users, wantUsers) || c.BranchPrefix != "work/" {
				t.Fatal("JSON owner or branch settings were not loaded")
			}
		})
	}
}
