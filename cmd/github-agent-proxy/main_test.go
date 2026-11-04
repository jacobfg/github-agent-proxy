package main

import (
	"bytes"
	"strings"
	"testing"
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
