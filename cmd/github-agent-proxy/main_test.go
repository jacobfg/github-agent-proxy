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
	t.Setenv("ALLOWED_USERS", "must-not-be-copied")
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
