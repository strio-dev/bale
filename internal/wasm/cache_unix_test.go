//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package wasm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strio-dev/bale/internal/wasmtest"
	"github.com/wago-org/wago"
)

func TestTrustedCache(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "cache")
	path := moduleFile(t, wasmtest.Command(`{"cached":true}`, 0))
	request := Request{path, []byte(`{}`)}
	options := Options{CacheDirectory: directory}
	for range 2 {
		output, err := Run(context.Background(), request, options)
		if err != nil || string(output) != `{"cached":true}` {
			t.Fatalf("result=%q error=%v", output, err)
		}
	}
	files, err := filepath.Glob(filepath.Join(directory, "*.native"))
	if err != nil || len(files) != 1 {
		t.Fatalf("entries=%v error=%v", files, err)
	}
	file := files[0]
	if err = os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = Run(context.Background(), request, options); err == nil {
		t.Fatal("insecure cache accepted")
	}
	if err = os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	entry, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = entry.Write([]byte("trailing"))
	if closeErr := entry.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if _, err = Run(context.Background(), request, options); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("corrupt cache was not rejected: %v", err)
	}
	if err = os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, []byte("invalid executable"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Run(context.Background(), request, options); err == nil {
		t.Fatal("invalid cache silently recompiled")
	}
}

func TestTrustedCacheRejectsLinksAndPermissions(t *testing.T) {
	root := t.TempDir()
	insecure := filepath.Join(root, "insecure")
	if err := os.Mkdir(insecure, 0755); err != nil {
		t.Fatal(err)
	}
	if cache, err := openTrustedCache(insecure); err == nil {
		cache.Close()
		t.Fatal("insecure directory accepted")
	}
	trusted := filepath.Join(root, "trusted")
	if err := os.Mkdir(trusted, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(trusted, link); err != nil {
		t.Fatal(err)
	}
	if cache, err := openTrustedCache(link); err == nil {
		cache.Close()
		t.Fatal("symlink cache directory accepted")
	}
	cache, err := openTrustedCache(trusted)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	target := filepath.Join(root, "outside")
	if err = os.WriteFile(target, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(target, filepath.Join(trusted, "key.native")); err != nil {
		t.Fatal(err)
	}
	if _, err = cache.load("key"); err == nil {
		t.Fatal("symlink cache artifact accepted")
	}
}

func TestCacheIdentityIncludesSourceAndConfiguration(t *testing.T) {
	cfg := wago.NewRuntimeConfig()
	first, err := compilerKey("first", cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := compilerKey("second", cfg)
	if err != nil || first == second {
		t.Fatal("source not included in cache key", err)
	}
	changed, err := compilerKey("first", cfg.WithMemoryLimitPages(4))
	if err != nil || first == changed {
		t.Fatal("limits not included in cache key", err)
	}
}
