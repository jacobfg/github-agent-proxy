package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	githubagentproxy "github.com/jacobfg/github-agent-proxy2"
)

func installSkill(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("install-skill", flag.ContinueOnError)
	flags.SetOutput(out)
	output := flags.String("output", "", "new skill directory, including the github-agent-proxy folder name")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(*output) == "" || flags.NArg() != 0 {
		return fmt.Errorf("usage: github-agent-proxy install-skill --output DIR")
	}
	dir := filepath.Clean(*output)
	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		return fmt.Errorf("create skill parent: %w", err)
	}
	// A new directory prevents overwriting an installed or customized skill,
	// including a destination that is a symlink.
	if err := os.Mkdir(dir, 0755); err != nil {
		return fmt.Errorf("create skill directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "SKILL.md"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("create skill: %w", err)
	}
	_, writeErr := io.WriteString(f, githubagentproxy.AgentSkill)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	_, err = fmt.Fprintln(out, filepath.Join(dir, "SKILL.md"))
	return err
}
