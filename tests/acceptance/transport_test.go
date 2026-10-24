package acceptance_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strio-dev/bale"
)

// TestRegistryTransportAcceptance tests the packaged client's TLS trust and
// Docker credential challenge handling. TLS and Basic auth terminate at a local
// frontend; the storage and OCI responses come from the real registry. It does
// not qualify either registry's native authentication or certificate settings.
func TestRegistryTransportAcceptance(t *testing.T) {
	names := []string{"BALE_ACCEPTANCE_BINARY", "BALE_ACCEPTANCE_DISTRIBUTION", "BALE_ACCEPTANCE_ZOT"}
	paths := make([]string, len(names))
	present := 0
	for i, name := range names {
		paths[i] = os.Getenv(name)
		if paths[i] != "" {
			present++
		}
	}
	if present == 0 {
		t.Skip("run the registry acceptance app to supply packaged Bale, Distribution and Zot")
	}
	if present != len(names) {
		t.Fatal("all BALE_ACCEPTANCE_BINARY, BALE_ACCEPTANCE_DISTRIBUTION and BALE_ACCEPTANCE_ZOT must be set")
	}
	for i, path := range paths {
		info, err := os.Stat(path)
		if err != nil || !filepath.IsAbs(path) || info.IsDir() || info.Mode()&0111 == 0 {
			t.Fatalf("%s must name an absolute executable: %q (%v)", names[i], path, err)
		}
	}
	for _, backend := range []struct{ name, binary string }{{"distribution", paths[1]}, {"zot", paths[2]}} {
		t.Run(backend.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			registry := startRegistry(t, ctx, backend.name, backend.binary, "TLS upstream")
			client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{registry.host}, HTTPClient: &http.Client{Timeout: 10 * time.Second}})
			if err != nil {
				t.Fatal(err)
			}
			subject := seed(t, ctx, client, registry.host, "transport", "transport-v1", false)
			dir := t.TempDir()
			configDir := filepath.Join(dir, "docker")
			if err := os.Mkdir(configDir, 0700); err != nil {
				t.Fatal(err)
			}
			recipe := filepath.Join(dir, "bale.yaml")
			writeFile(t, recipe, []byte("include:\n  - ref: "+subject.Repository+":stable\n"))
			// Fixture creation is local HTTP; every transport assertion below uses
			// HTTPS without --plain-http or an insecure verification override.
			command := cli{t: t, ctx: ctx, binary: paths[0], env: []string{"DOCKER_CONFIG=" + configDir, "XDG_DATA_HOME=" + filepath.Join(dir, "data")}, hosts: []string{registry.host}}
			created := decode[bale.CreateResult](t, command.run("transport fixture", "create", "--registry", registry.host+"/bundles:transport", "--file", recipe, "--format", "json"))
			upstream, err := url.Parse("http://" + registry.host)
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(upstream)
			var denied, accepted atomic.Int64
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				if !ok || user != "bale-acceptance" || password != "fixture-password" {
					denied.Add(1)
					w.Header().Set("WWW-Authenticate", `Basic realm="bale-acceptance"`)
					http.Error(w, "authentication required", http.StatusUnauthorized)
					return
				}
				accepted.Add(1)
				proxy.ServeHTTP(w, r)
			}))
			// Certificate rejection is intentional and asserted by the CLI result.
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.StartTLS()
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "https://")
			ca := filepath.Join(dir, "ca.pem")
			writeFile(t, ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
			emptyCertDir := filepath.Join(dir, "certificates")
			if err := os.Mkdir(emptyCertDir, 0700); err != nil {
				t.Fatal(err)
			}
			run := func(name, auth string, trust bool, wantError string) []byte {
				t.Helper()
				credentials := map[string]any{"auths": map[string]any{}}
				if auth != "" {
					credentials["auths"] = map[string]any{host: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(auth))}}
				}
				raw, err := json.Marshal(credentials)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(configDir, "config.json"), raw)
				commandCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				command := exec.CommandContext(commandCtx, paths[0], "pull", "--layout", host+"/bundles:transport", filepath.Join(dir, name), "--format", "json")
				// Isolate credentials, proxy settings and trust inputs from the host.
				command.Env = []string{"DOCKER_CONFIG=" + configDir, "SSL_CERT_DIR=" + emptyCertDir, "XDG_DATA_HOME=" + filepath.Join(dir, "data")}
				if trust {
					command.Env = append(command.Env, "SSL_CERT_FILE="+ca)
				} else {
					command.Env = append(command.Env, "SSL_CERT_FILE="+filepath.Join(dir, "missing-ca.pem"))
				}
				var stdout, stderr bytes.Buffer
				command.Stdout, command.Stderr = &stdout, &stderr
				err = command.Run()
				if wantError == "" {
					if err != nil {
						t.Fatalf("%s: %v\n%s", name, err, tail(stderr.Bytes(), 16000))
					}
				} else if err == nil || !strings.Contains(stderr.String(), wantError) {
					t.Fatalf("%s: expected failure containing %q, got %v\n%s", name, wantError, err, tail(stderr.Bytes(), 16000))
				}
				t.Logf("%s: expected transport outcome confirmed", name)
				return stdout.Bytes()
			}
			run("anonymous", "", true, "basic credential not found")
			run("wrong-credentials", "bale-acceptance:wrong-password", true, "401")
			if accepted.Load() != 0 || denied.Load() == 0 {
				t.Fatalf("invalid credentials reached registry: accepted=%d denied=%d", accepted.Load(), denied.Load())
			}
			before := denied.Load()
			run("untrusted-certificate", "bale-acceptance:fixture-password", false, "x509")
			if accepted.Load() != 0 || denied.Load() != before {
				t.Fatal("untrusted TLS connection reached HTTP handler")
			}
			pulled := decode[bale.PullResult](t, run("trusted-authenticated", "bale-acceptance:fixture-password", true, ""))
			if pulled.Descriptor.Digest != created.Descriptor.Digest || accepted.Load() == 0 {
				t.Fatalf("authenticated pull: got digest %s, want %s; authorized requests=%d", pulled.Descriptor.Digest, created.Descriptor.Digest, accepted.Load())
			}
		})
	}
}
