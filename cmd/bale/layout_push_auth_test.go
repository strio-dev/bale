package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLayoutPushUsesRegistryCredentials(t *testing.T) {
	layout, _, _ := cliBundle(t)
	for _, source := range []string{"docker", "environment", "authfile"} {
		t.Run(source, func(t *testing.T) {
			var authenticated atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if user, password, ok := r.BasicAuth(); !ok || user != "transfer" || password != "fixture" {
					w.Header().Set("WWW-Authenticate", `Basic realm="push"`)
					http.Error(w, "credentials required", http.StatusUnauthorized)
					return
				}
				authenticated.Add(1)
				// Stop after checking the actual registry authentication path; this
				// test does not need to emulate a writable registry.
				http.Error(w, "publication disabled by fixture", http.StatusForbidden)
			}))
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "http://")
			docker := t.TempDir()
			t.Setenv("DOCKER_CONFIG", docker)
			t.Setenv("REGISTRY_AUTH_FILE", "")
			config := fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, base64.StdEncoding.EncodeToString([]byte("transfer:fixture")))
			authfile := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(authfile, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			dockerConfig := "invalid"
			args := []string{"push", "--layout", layout, host + "/release:bundle", "--plain-http", host, "--quiet"}
			switch source {
			case "docker":
				dockerConfig = config
			case "environment":
				t.Setenv("REGISTRY_AUTH_FILE", authfile)
			case "authfile":
				args = append(args, "--authfile", authfile)
			}
			if err := os.WriteFile(filepath.Join(docker, "config.json"), []byte(dockerConfig), 0600); err != nil {
				t.Fatal(err)
			}
			var out, errs bytes.Buffer
			if code := run(args, &out, &errs); code != 125 || authenticated.Load() == 0 || out.Len() != 0 {
				t.Fatalf("layout push did not use %s credentials: exit=%d authenticated=%d stdout=%s stderr=%s", source, code, authenticated.Load(), &out, &errs)
			}
		})
	}
}
