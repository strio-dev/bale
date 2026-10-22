// bale-flux serves configured Bale selections to Flux Operator ExternalService providers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	relocate "github.com/strio-dev/bale/plugins/helm-relocate"
	"github.com/strio-dev/bale/plugins/helm-relocate/flux"
)

type flags struct {
	config, listen, cert, key, token string
	registry                         relocate.RegistryOptions
}

func parse(args []string, output io.Writer) (flags, error) {
	var o flags
	f := flag.NewFlagSet("bale-flux", flag.ContinueOnError)
	f.SetOutput(output)
	f.StringVar(&o.config, "config", "", "configured release YAML (required)")
	f.StringVar(&o.listen, "listen", "127.0.0.1:8080", "HTTP listen address")
	f.StringVar(&o.cert, "tls-cert-file", "", "server TLS certificate")
	f.StringVar(&o.key, "tls-key-file", "", "server TLS private key")
	f.StringVar(&o.token, "bearer-token-file", "", "file containing the ExternalService bearer token")
	f.StringVar(&o.registry.RegistryConfig, "registry-config", "", "Docker/Helm registry credential configuration")
	f.StringVar(&o.registry.CAFile, "ca-file", "", "registry CA certificate")
	f.StringVar(&o.registry.CertFile, "cert-file", "", "registry client TLS certificate")
	f.StringVar(&o.registry.KeyFile, "key-file", "", "registry client TLS private key")
	f.BoolVar(&o.registry.InsecureSkipVerify, "insecure-skip-tls-verify", false, "skip registry certificate verification")
	f.Func("plain-http", "registry host allowed to use HTTP (repeatable)", func(host string) error {
		if host == "" || strings.ContainsAny(host, "/ \t\r\n") {
			return fmt.Errorf("plain-http requires a registry host")
		}
		o.registry.PlainHTTPRegistries = append(o.registry.PlainHTTPRegistries, host)
		return nil
	})
	if err := f.Parse(args); err != nil {
		return o, err
	}
	if f.NArg() != 0 {
		return o, fmt.Errorf("unexpected positional arguments")
	}
	if o.config == "" {
		return o, fmt.Errorf("--config is required")
	}
	if (o.cert == "") != (o.key == "") {
		return o, fmt.Errorf("server TLS certificate and key must be provided together")
	}
	return o, nil
}
func readToken(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 16<<10+1))
	if err != nil {
		return "", err
	}
	if len(raw) > 16<<10 {
		return "", fmt.Errorf("bearer token exceeds limit")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("bearer token must be nonempty without whitespace")
	}
	return token, nil
}
func run(ctx context.Context, args []string, output io.Writer) error {
	o, err := parse(args, output)
	if err != nil {
		return err
	}
	f, err := os.Open(o.config)
	if err != nil {
		return err
	}
	config, err := flux.LoadConfig(f)
	_ = f.Close()
	if err != nil {
		return err
	}
	token, err := readToken(o.token)
	if err != nil {
		return err
	}
	client, err := relocate.NewRegistryClient(o.registry)
	if err != nil {
		return err
	}
	handler, err := flux.NewHandler(config, client, flux.HandlerOptions{
		BearerToken: token,
		Timeout:     30 * time.Second,
		OnError: func(release string, err error) {
			_, _ = fmt.Fprintf(output, "bale-flux: release %s: %v\n", release, err)
		},
	})
	if err != nil {
		return err
	}
	server := &http.Server{Addr: o.listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() {
		if o.cert != "" {
			done <- server.ListenAndServeTLS(o.cert, o.key)
		} else {
			done <- server.ListenAndServe()
		}
	}()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "bale-flux:", err)
		os.Exit(1)
	}
}
