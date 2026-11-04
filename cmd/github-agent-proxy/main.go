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
	if len(args) != 0 {
		return fmt.Errorf("usage: github-agent-proxy [--version | init-ca --cert PATH --key PATH]")
	}
	c, err := proxy.FromEnv()
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
