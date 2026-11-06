package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobfg/github-agent-proxy2/internal/proxy"
)

func TestRunRequiresPolicyFile(t *testing.T) {
	t.Setenv("POLICY_FILE", "")
	t.Setenv("GITHUB_TOKEN_FILE", "/nonexistent/token")
	if err := runArgs(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "POLICY_FILE is required") {
		t.Fatalf("startup did not reject missing policy first: %v", err)
	}
}

func serviceEnvironment(t *testing.T) (string, string) {
	t.Helper()
	policy := filepath.Join(t.TempDir(), "policy.json")
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(policy, []byte(proxy.DefaultPolicy()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("file-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"POLICY_FILE": policy, "GITHUB_TOKEN_FILE": token, "GITHUB_TOKEN": "",
		"TLS_CERT_FILE": "env-cert", "TLS_KEY_FILE": "env-key",
		"INTERCEPT_CA_CERT_FILE": "env-ca", "INTERCEPT_CA_KEY_FILE": "env-ca-key",
		"LISTEN_ADDR": "127.0.0.1:9443", "ALLOW_CONTAINER_BIND": "", "MAX_PUSH_BYTES": "2048",
	} {
		t.Setenv(key, value)
	}
	return policy, token
}

func TestServiceFlagPrecedence(t *testing.T) {
	policy, token := serviceEnvironment(t)
	c, err := serviceConfig(nil, &bytes.Buffer{})
	if err != nil || c.Policy == nil || c.Token != "file-token" || c.Listen != "127.0.0.1:9443" || c.CertFile != "env-cert" || c.KeyFile != "env-key" || c.CACertFile != "env-ca" || c.CAKeyFile != "env-ca-key" || c.MaxPushBytes != 2048 {
		t.Fatalf("environment configuration: error=%v", err)
	}
	// Overrides must take effect before reading files or validating values.
	t.Setenv("POLICY_FILE", "/nonexistent/policy")
	t.Setenv("GITHUB_TOKEN_FILE", "/nonexistent/token")
	t.Setenv("MAX_PUSH_BYTES", "invalid")
	c, err = serviceConfig([]string{
		"--policy-file", policy, "--github-token-file", token,
		"--listen-addr", "0.0.0.0:8443", "--allow-container-bind",
		"--tls-cert-file", "flag-cert", "--tls-key-file", "flag-key",
		"--intercept-ca-cert-file", "flag-ca", "--intercept-ca-key-file", "flag-ca-key",
		"--max-push-bytes", "4096",
	}, &bytes.Buffer{})
	if err != nil || c.Policy == nil || c.Token != "file-token" || c.Listen != "0.0.0.0:8443" || c.CertFile != "flag-cert" || c.KeyFile != "flag-key" || c.CACertFile != "flag-ca" || c.CAKeyFile != "flag-ca-key" || c.MaxPushBytes != 4096 {
		t.Fatalf("flag configuration: error=%v", err)
	}
	if os.Getenv("POLICY_FILE") != "/nonexistent/policy" || os.Getenv("MAX_PUSH_BYTES") != "invalid" || os.Getenv("TLS_CERT_FILE") != "env-cert" || os.Getenv("ALLOW_CONTAINER_BIND") != "" {
		t.Fatal("flag parsing changed the environment")
	}
}

func TestServiceFlagsValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"empty policy overrides env", []string{"--policy-file="}, "POLICY_FILE is required"},
		{"missing policy", []string{"--policy-file", "/nonexistent/policy"}, "read policy"},
		{"empty TLS overrides env", []string{"--tls-key-file="}, "TLS_KEY_FILE"},
		{"public bind", []string{"--listen-addr", "0.0.0.0:8443"}, "loopback"},
		{"disable container bind", []string{"--listen-addr", "0.0.0.0:8443", "--allow-container-bind=false"}, "loopback"},
		{"invalid push size", []string{"--max-push-bytes", "invalid"}, "MAX_PUSH_BYTES"},
		{"too small push size", []string{"--max-push-bytes", "100"}, "MAX_PUSH_BYTES"},
		{"unknown flag", []string{"--unknown"}, "usage:"},
		{"missing value", []string{"--policy-file"}, "usage:"},
		{"unexpected positional", []string{"extra"}, "usage:"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			serviceEnvironment(t)
			if tt.name == "disable container bind" {
				t.Setenv("ALLOW_CONTAINER_BIND", "1")
			}
			if _, err := serviceConfig(tt.args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q, got %v", tt.want, err)
			}
		})
	}
}

func TestServiceMixedConfiguration(t *testing.T) {
	_, token := serviceEnvironment(t)
	c, err := serviceConfig([]string{"--listen-addr", "[::1]:8443"}, &bytes.Buffer{})
	if err != nil || c.Listen != "[::1]:8443" || c.Token != "file-token" || c.CertFile != "env-cert" {
		t.Fatalf("mixed configuration: %v", err)
	}
	t.Setenv("GITHUB_TOKEN", "environment-token")
	if _, err := serviceConfig([]string{"--github-token-file", token}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "set only one") {
		t.Fatalf("token sources conflict was not rejected: %v", err)
	}
	c, err = serviceConfig([]string{"--github-token-file="}, &bytes.Buffer{})
	if err != nil || c.Token != "environment-token" {
		t.Fatalf("explicitly clearing token file: %v", err)
	}
}

func TestHelpDoesNotRequireServiceConfiguration(t *testing.T) {
	t.Setenv("POLICY_FILE", "/nonexistent/policy")
	t.Setenv("GITHUB_TOKEN_FILE", "/nonexistent/token")
	t.Setenv("GITHUB_TOKEN", "secret-not-for-help")
	var out bytes.Buffer
	if err := runArgs([]string{"--help"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "-policy-file") || !strings.Contains(out.String(), "init-policy") || strings.Contains(out.String(), "secret-not-for-help") {
		t.Fatal("unexpected help output")
	}
}

func TestVersionDoesNotRequireServiceConfiguration(t *testing.T) {
	// Package verification must work before configuring a PAT, policy or TLS.
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN_FILE", "/nonexistent/token")
	t.Setenv("POLICY_FILE", "/nonexistent/policy.json")
	for _, args := range [][]string{{"--version"}, {"version"}} {
		var out bytes.Buffer
		if err := runArgs(args, &out); err != nil {
			t.Fatal(err)
		}
		if out.String() != "github-agent-proxy "+version+"\n" {
			t.Fatalf("unexpected version: %q", out.String())
		}
	}
}

func TestInitPolicy(t *testing.T) {
	t.Setenv("GITHUB_TOKEN_FILE", "/nonexistent/token")
	t.Setenv("POLICY_FILE", "/nonexistent/policy.json")
	for _, args := range [][]string{{"init-policy"}, {"init-policy", "--output", "-"}} {
		var out bytes.Buffer
		if err := runArgs(args, &out); err != nil {
			t.Fatal(err)
		}
		if out.String() != proxy.DefaultPolicy() {
			t.Fatalf("unexpected policy: %s", out.String())
		}
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	var out bytes.Buffer
	if err := runArgs([]string{"init-policy", "--output", path}, &out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != proxy.DefaultPolicy() || out.Len() != 0 {
		t.Fatalf("file output: %q, stdout=%q, error=%v", data, out.String(), err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("policy permissions: %v, %v", info, err)
	}
	if err := os.WriteFile(path, []byte("existing policy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runArgs([]string{"init-policy", "--output", path}, &out); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing file accepted: %v", err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "existing policy" {
		t.Fatal("existing policy was modified", err)
	}
	link := filepath.Join(t.TempDir(), "policy.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := runArgs([]string{"init-policy", "--output", link}, &out); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing symlink accepted: %v", err)
	}
	for _, args := range [][]string{{"init-policy", "extra"}, {"init-policy", "--unknown"}, {"init-policy", "--output"}, {"init-policy", "--output", ""}} {
		if err := runArgs(args, &out); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}

func TestUnexpectedArgumentsDoNotStartService(t *testing.T) {
	for _, args := range [][]string{{"--version", "extra"}, {"--licenses", "extra"}, {"unknown"}} {
		if err := runArgs(args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("unexpected arguments accepted: %v", err)
		}
	}
}

func TestLicensesDoesNotRequireServiceConfiguration(t *testing.T) {
	t.Setenv("GITHUB_TOKEN_FILE", "/nonexistent/token")
	t.Setenv("POLICY_FILE", "/nonexistent/policy.json")
	var out bytes.Buffer
	if err := runArgs([]string{"--licenses"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, notice := range []string{"github.com/vektah/gqlparser/v2", "Copyright (c) 2018 Adam Scarr", "THE SOFTWARE IS PROVIDED"} {
		if !strings.Contains(out.String(), notice) {
			t.Fatalf("missing notice: %s", notice)
		}
	}
}
