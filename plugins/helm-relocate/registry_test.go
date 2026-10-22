package relocate

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryCredentialPrecedenceAndHostScope(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auths":{"registry.example":{"auth":"ZG9ja2VyOnBhc3M="},"other.example":{"auth":"b3RoZXI6cGFzcw=="}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	primary := filepath.Join(dir, "helm.json")
	if err := os.WriteFile(primary, []byte(`{"auths":{"registry.example":{"auth":"aGVsbTpwYXNz"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	options, err := registryClientOptions(RegistryOptions{RegistryConfig: primary})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := options.Credentials(context.Background(), "registry.example")
	if err != nil || cred.Username != "helm" {
		t.Fatalf("Helm precedence: %v %v", cred, err)
	}
	cred, err = options.Credentials(context.Background(), "other.example")
	if err != nil || cred.Username != "other" {
		t.Fatalf("Docker fallback: %v %v", cred, err)
	}
	options, err = registryClientOptions(RegistryOptions{RegistryConfig: primary, Username: "explicit", Password: "secret", CredentialHost: "registry.example"})
	if err != nil {
		t.Fatal(err)
	}
	cred, err = options.Credentials(context.Background(), "registry.example")
	if err != nil || cred.Username != "explicit" {
		t.Fatalf("explicit credentials: %v %v", cred, err)
	}
	cred, err = options.Credentials(context.Background(), "other.example")
	if err != nil || cred.Username != "other" {
		t.Fatal("credentials leaked to another host")
	}
}
func TestRegistryTLSPolicy(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	for _, o := range []RegistryOptions{{CertFile: "cert"}, {KeyFile: "key"}, {Username: "user"}, {CAFile: "missing"}} {
		if _, err := registryClientOptions(o); err == nil {
			t.Fatalf("invalid policy accepted: %+v", o)
		}
	}
	originalTLS := http.DefaultTransport.(*http.Transport).TLSClientConfig
	options, err := registryClientOptions(RegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	transport := options.HTTPClient.Transport.(*http.Transport)
	if transport == http.DefaultTransport || transport.TLSClientConfig.InsecureSkipVerify || transport.Proxy == nil || options.HTTPClient.Timeout == 0 || transport.ResponseHeaderTimeout == 0 {
		t.Fatal("insecure or unbounded HTTP defaults")
	}
	options, err = registryClientOptions(RegistryOptions{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if !options.HTTPClient.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify {
		t.Fatal("explicit insecure setting ignored")
	}
	if originalTLS != nil && http.DefaultTransport.(*http.Transport).TLSClientConfig != originalTLS {
		t.Fatal("mutated global transport")
	}
}

func TestRegistryCustomCAConnection(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	defaults, err := registryClientOptions(RegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if response, err := defaults.HTTPClient.Get(server.URL); err == nil {
		_ = response.Body.Close()
		t.Fatal("untrusted registry certificate accepted")
	}
	trusted, err := registryClientOptions(RegistryOptions{CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	response, err := trusted.HTTPClient.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("custom CA failed: %d", response.StatusCode)
	}
	if err := os.WriteFile(ca, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := registryClientOptions(RegistryOptions{CAFile: ca}); err == nil {
		t.Fatal("invalid certificate accepted")
	}
}
