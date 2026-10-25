package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitAuthFileSelection(t *testing.T) {
	badDocker := t.TempDir()
	t.Setenv("DOCKER_CONFIG", badDocker)
	if err := os.WriteFile(filepath.Join(badDocker, "config.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	writeAuth := func(user string) string {
		name := filepath.Join(t.TempDir(), "auth.json")
		data := fmt.Sprintf(`{"auths":{"registry.example.com":{"auth":%q}}}`, base64.StdEncoding.EncodeToString([]byte(user+":password")))
		if err := os.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		return name
	}
	envFile, explicitFile := writeAuth("environment"), writeAuth("explicit")
	t.Setenv("REGISTRY_AUTH_FILE", envFile)
	for _, tc := range []struct{ file, user string }{{"", "environment"}, {explicitFile, "explicit"}} {
		credential, err := lazyDockerCredentials(tc.file)(t.Context(), "registry.example.com")
		if err != nil || credential.Username != tc.user || credential.Password != "password" {
			t.Fatalf("auth file selection: user=%q error=%v", credential.Username, err)
		}
	}
	if _, err := lazyDockerCredentials(filepath.Join(t.TempDir(), "missing"))(t.Context(), "registry.example.com"); err == nil {
		t.Fatal("missing explicit auth file fell back to ambient credentials")
	}
}

func TestAuthFileInitializationIsLazy(t *testing.T) {
	// Constructing a client stays local; malformed credentials are read only when
	// registry authentication is requested.
	name := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(name, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newCLIClient(commandOptions{authFile: name}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := lazyDockerCredentials(name)(t.Context(), "registry.example.com"); err == nil {
		t.Fatal("malformed authentication file was accepted for a registry operation")
	}
}
