package proxy

import (
	"strings"
	"testing"
)

func TestConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name, key, value string
		valid            bool
	}{
		{"defaults", "ALLOWED_ORGS", "acme, Other", true},
		{"read only without owners", "ALLOWED_ORGS", "", true},
		{"missing prefix", "BRANCH_PREFIX", "", false},
		{"no namespace boundary", "BRANCH_PREFIX", "agent", false},
		{"invalid prefix", "BRANCH_PREFIX", "agent/../", false},
		{"missing token", "GITHUB_TOKEN", "", false},
		{"missing TLS", "TLS_KEY_FILE", "", false},
		{"public bind", "LISTEN_ADDR", "0.0.0.0:8443", false},
		{"IPv6 loopback", "LISTEN_ADDR", "[::1]:8443", true},
		{"invalid size", "MAX_PUSH_BYTES", "0", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range map[string]string{"ALLOWED_ORGS": "acme", "ALLOWED_USERS": "", "BRANCH_PREFIX": "agent/", "GITHUB_TOKEN": "secret", "GITHUB_TOKEN_FILE": "", "TLS_CERT_FILE": "cert.pem", "TLS_KEY_FILE": "key.pem", "LISTEN_ADDR": "", "ALLOW_CONTAINER_BIND": "", "MAX_PUSH_BYTES": ""} {
				t.Setenv(key, value)
			}
			t.Setenv(tt.key, tt.value)
			_, err := FromEnv()
			if (err == nil) != tt.valid {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestOwnerAllowlistConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name, orgs, users string
		valid             bool
	}{
		{"orgs only", " Acme, OTHER ", "", true},
		{"users only", "", " JacobFG, Bot ", true},
		{"both", "Acme", "JacobFG", true},
		{"neither", "", "", true},
		{"empty entries", " , ", " , ", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range map[string]string{"ALLOWED_ORGS": tt.orgs, "ALLOWED_USERS": tt.users, "BRANCH_PREFIX": "agent/", "GITHUB_TOKEN": "secret", "GITHUB_TOKEN_FILE": "", "TLS_CERT_FILE": "cert.pem", "TLS_KEY_FILE": "key.pem", "LISTEN_ADDR": "", "MAX_PUSH_BYTES": "", "ALLOW_CONTAINER_BIND": ""} {
				t.Setenv(k, v)
			}
			c, err := FromEnv()
			if (err == nil) != tt.valid {
				t.Fatalf("error=%v", err)
			}
			if strings.Contains(tt.orgs, "Acme") && !c.Organizations["acme"] {
				t.Fatal("organisation login was not normalized")
			}
			if strings.Contains(tt.users, "JacobFG") && !c.Users["jacobfg"] {
				t.Fatal("user login was not normalized")
			}
			if !strings.Contains(tt.orgs, "Acme") && !strings.Contains(tt.users, "JacobFG") && (len(c.Organizations) != 0 || len(c.Users) != 0) {
				t.Fatal("empty owner lists authorized an owner")
			}
		})
	}
}
