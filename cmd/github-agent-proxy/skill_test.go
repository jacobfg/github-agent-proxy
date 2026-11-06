package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	githubagentproxy "github.com/jacobfg/github-agent-proxy2"
)

func TestInstallSkill(t *testing.T) {
	// Installation is usable before service setup and never reads its secrets.
	t.Setenv("GITHUB_TOKEN_FILE", "/nonexistent/token")
	t.Setenv("POLICY_FILE", "/nonexistent/policy")
	t.Setenv("TLS_KEY_FILE", "/nonexistent/key")
	var shown bytes.Buffer
	if err := runArgs([]string{"show-skill"}, &shown); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "skills", "github-agent-proxy")
	var out bytes.Buffer
	if err := runArgs([]string{"install-skill", "--output", dir}, &out); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "SKILL.md")
	contents, err := os.ReadFile(file)
	if err != nil || string(contents) != githubagentproxy.AgentSkill || len(contents) == 0 {
		t.Fatalf("installed skill differs from embedded resource: %v", err)
	}
	if !bytes.Equal(contents, shown.Bytes()) {
		t.Fatal("show-skill output differs from installed skill")
	}
	if strings.TrimSpace(out.String()) != file {
		t.Fatalf("unexpected installation output: %q", out.String())
	}
	if err := os.WriteFile(file, []byte("customized skill"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runArgs([]string{"install-skill", "--output", dir}, &out); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing skill accepted: %v", err)
	}
	contents, err = os.ReadFile(file)
	if err != nil || string(contents) != "customized skill" {
		t.Fatal("existing skill was changed", err)
	}
}

func TestInstallSkillRejectsExistingPaths(t *testing.T) {
	for _, kind := range []string{"file", "directory", "symlink", "dangling symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "github-agent-proxy")
			target := filepath.Join(root, "target")
			var err error
			switch kind {
			case "file":
				err = os.WriteFile(dir, []byte("keep"), 0600)
			case "directory":
				err = os.Mkdir(dir, 0755)
			case "symlink":
				if err = os.Mkdir(target, 0755); err != nil {
					t.Fatal(err)
				}
				fallthrough
			case "dangling symlink":
				err = os.Symlink(target, dir)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := runArgs([]string{"install-skill", "--output", dir}, &bytes.Buffer{}); !errors.Is(err, os.ErrExist) {
				t.Fatalf("existing destination accepted: %v", err)
			}
			if kind == "symlink" {
				entries, err := os.ReadDir(target)
				if err != nil || len(entries) != 0 {
					t.Fatalf("symlink target was modified: %v", err)
				}
			}
		})
	}
}

func TestInstallSkillArguments(t *testing.T) {
	t.Setenv("POLICY_FILE", "/nonexistent/policy")
	for _, args := range [][]string{
		{"install-skill"}, {"install-skill", "--output"},
		{"install-skill", "--output="}, {"install-skill", "--unknown"},
		{"show-skill", "extra"}, {"show-skill", "--output", "unused"},
	} {
		if err := runArgs(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
	dir := filepath.Join(t.TempDir(), "unused")
	if err := runArgs([]string{"install-skill", "--output", dir, "extra"}, &bytes.Buffer{}); err == nil {
		t.Fatal("unexpected positional argument accepted")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid invocation created output", err)
	}
	for _, args := range [][]string{{"install-skill", "--help"}, {"--help"}} {
		var out bytes.Buffer
		if err := runArgs(args, &out); err != nil || !strings.Contains(out.String(), "install-skill") {
			t.Fatalf("skill help unavailable: %v", err)
		}
	}
}
