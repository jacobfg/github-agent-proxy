package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jacobfg/github-agent-proxy2"
	"github.com/jacobfg/github-agent-proxy2/internal/proxy"
)

// Set by release builds; local development builds deliberately identify as dev.
var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	return runArgs(os.Args[1:], os.Stdout)
}

func runArgs(args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "show-skill" {
		if len(args) != 1 {
			return fmt.Errorf("usage: github-agent-proxy show-skill")
		}
		_, err := io.WriteString(out, githubagentproxy.AgentSkill)
		return err
	}
	if len(args) > 0 && args[0] == "install-skill" {
		return installSkill(args[1:], out)
	}
	if len(args) > 0 && args[0] == "init-policy" {
		flags := flag.NewFlagSet("init-policy", flag.ContinueOnError)
		output := flags.String("output", "-", "output file; - writes JSON to stdout")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("usage: github-agent-proxy init-policy [--output PATH]")
		}
		if *output == "-" {
			_, err := io.WriteString(out, proxy.DefaultPolicy())
			return err
		}
		f, err := os.OpenFile(*output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("create policy: %w", err)
		}
		_, writeErr := io.WriteString(f, proxy.DefaultPolicy())
		closeErr := f.Close()
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	if len(args) == 1 && args[0] == "--licenses" {
		_, err := io.WriteString(out, githubagentproxy.ThirdPartyNotices)
		return err
	}
	if len(args) == 1 && (args[0] == "--version" || args[0] == "version") {
		_, err := fmt.Fprintf(out, "github-agent-proxy %s\n", version)
		return err
	}
	if len(args) > 0 && args[0] == "init-ca" {
		flags := flag.NewFlagSet("init-ca", flag.ContinueOnError)
		cert := flags.String("cert", "", "public CA certificate path")
		key := flags.String("key", "", "private signing key path")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *cert == "" || *key == "" || flags.NArg() != 0 {
			return fmt.Errorf("usage: github-agent-proxy init-ca --cert PATH --key PATH")
		}
		return proxy.GenerateCA(*cert, *key)
	}
	c, err := serviceConfig(args, out)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return err
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	p, err := proxy.New(ctx, c, tlsConfig)
	if err != nil {
		return err
	}
	defer p.Close()
	s := &http.Server{Addr: c.Listen, Handler: p, TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Minute, WriteTimeout: 10 * time.Minute, IdleTimeout: time.Minute, MaxHeaderBytes: 64 << 10}
	done := make(chan error, 1)
	go func() {
		log.Printf("HTTPS forward and GitHub policy proxy listening on https://%s", c.Listen)
		done <- s.ListenAndServeTLS("", "")
	}()
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		p.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.Shutdown(shutdown)
	}
}

// serviceConfig merges flags into an environment snapshot without changing the
// process environment. Parsing precedes policy, token and certificate reads.
func serviceConfig(args []string, out io.Writer) (proxy.Config, error) {
	options := proxy.RuntimeOptionsFromEnv()
	flags := flag.NewFlagSet("github-agent-proxy", flag.ContinueOnError)
	flags.SetOutput(out)
	flags.Usage = func() {
		fmt.Fprintln(out, "usage: github-agent-proxy [options]")
		fmt.Fprintln(out, "       github-agent-proxy --version | --licenses | init-policy [--output PATH] | init-ca --cert PATH --key PATH")
		fmt.Fprintln(out, "       github-agent-proxy install-skill --output DIR")
		fmt.Fprintln(out, "       github-agent-proxy show-skill")
		fmt.Fprintln(out, "Explicit flags override their environment variables. A policy file is required.")
		flags.PrintDefaults()
	}
	flags.StringVar(&options.PolicyFile, "policy-file", options.PolicyFile, "JSON policy path (POLICY_FILE; required)")
	flags.StringVar(&options.Listen, "listen-addr", options.Listen, "listener address (LISTEN_ADDR; default 127.0.0.1:8443)")
	flags.StringVar(&options.TokenFile, "github-token-file", options.TokenFile, "PAT file path (GITHUB_TOKEN_FILE)")
	flags.StringVar(&options.CertFile, "tls-cert-file", options.CertFile, "listener certificate path (TLS_CERT_FILE)")
	flags.StringVar(&options.KeyFile, "tls-key-file", options.KeyFile, "listener private key path (TLS_KEY_FILE)")
	flags.StringVar(&options.CACertFile, "intercept-ca-cert-file", options.CACertFile, "signing CA certificate path (INTERCEPT_CA_CERT_FILE)")
	flags.StringVar(&options.CAKeyFile, "intercept-ca-key-file", options.CAKeyFile, "signing CA private key path (INTERCEPT_CA_KEY_FILE)")
	flags.StringVar(&options.MaxPushBytes, "max-push-bytes", options.MaxPushBytes, "maximum receive-pack bytes (MAX_PUSH_BYTES; default 1073741824)")
	flags.BoolVar(&options.AllowContainerBind, "allow-container-bind", options.AllowContainerBind, "permit a non-loopback listener in an isolated container (ALLOW_CONTAINER_BIND=1)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return proxy.Config{}, err
		}
		return proxy.Config{}, fmt.Errorf("usage: github-agent-proxy [options]: %w", err)
	}
	if flags.NArg() != 0 {
		return proxy.Config{}, fmt.Errorf("usage: github-agent-proxy [options]; unexpected positional arguments")
	}
	return proxy.FromOptions(options)
}
