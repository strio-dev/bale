package relocate

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"

	bale "github.com/strio-dev/bale"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

// RegistryOptions configures registry access without sharing credentials across hosts.
type RegistryOptions struct {
	RegistryConfig, CAFile, CertFile, KeyFile, Username, Password, CredentialHost string
	PlainHTTPRegistries                                                           []string
	InsecureSkipVerify                                                            bool
}

func registryClientOptions(o RegistryOptions) (bale.ClientOptions, error) {
	if (o.CertFile == "") != (o.KeyFile == "") {
		return bale.ClientOptions{}, fmt.Errorf("client certificate and key must be provided together")
	}
	if (o.Username != "" || o.Password != "") && o.CredentialHost == "" {
		return bale.ClientOptions{}, fmt.Errorf("explicit credentials require a registry host")
	}
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{DetectDefaultNativeStore: true})
	if err != nil {
		return bale.ClientOptions{}, err
	}
	var combined credentials.Store = store
	if o.RegistryConfig != "" {
		primary, err := credentials.NewStore(o.RegistryConfig, credentials.StoreOptions{})
		if err != nil {
			return bale.ClientOptions{}, err
		}
		combined = credentials.NewStoreWithFallbacks(primary, store)
	}
	lookup := credentials.Credential(combined)
	credential := func(ctx context.Context, host string) (auth.Credential, error) {
		if host == o.CredentialHost && (o.Username != "" || o.Password != "") {
			return auth.Credential{Username: o.Username, Password: o.Password}, nil
		}
		return lookup(ctx, host)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: o.InsecureSkipVerify} // #nosec G402 -- explicitly selected by the caller.
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return bale.ClientOptions{}, fmt.Errorf("read registry CA: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return bale.ClientOptions{}, fmt.Errorf("load system roots: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return bale.ClientOptions{}, fmt.Errorf("registry CA contains no certificates")
		}
		tlsConfig.RootCAs = roots
	}
	if o.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile)
		if err != nil {
			return bale.ClientOptions{}, fmt.Errorf("load registry client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	transport.TLSClientConfig = tlsConfig
	transport.ResponseHeaderTimeout = 30 * time.Second
	return bale.ClientOptions{Credentials: credential, PlainHTTPRegistries: append([]string(nil), o.PlainHTTPRegistries...), HTTPClient: &http.Client{Transport: transport, Timeout: 2 * time.Minute}}, nil
}

// NewRegistryClient uses Helm credentials first, Docker credentials as fallback,
// and host-scoped explicit credentials when supplied.
func NewRegistryClient(o RegistryOptions) (*bale.Client, error) {
	opts, err := registryClientOptions(o)
	if err != nil {
		return nil, err
	}
	return bale.NewClient(opts)
}
