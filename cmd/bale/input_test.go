package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withInput(t *testing.T, input string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = previous; _ = f.Close() })
}

func TestReadInputFileStdinAndLimits(t *testing.T) {
	for _, stdin := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "stdin"}[stdin], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input")
			if stdin {
				withInput(t, "declaration")
				path = "-"
			} else if err := os.WriteFile(path, []byte("declaration"), 0600); err != nil {
				t.Fatal(err)
			}
			if raw, err := readInputFileLimit(path, 11); err != nil || string(raw) != "declaration" {
				t.Fatalf("read exact limit: %q: %v", raw, err)
			}
		})
	}
	withInput(t, strings.Repeat("x", 12))
	if _, err := readInputFileLimit("-", 11); err == nil || !strings.Contains(err.Error(), "exceeds 11 bytes") {
		t.Fatalf("stdin limit: %v", err)
	}
	path := filepath.Join(t.TempDir(), "oversize")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 12)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readInputFileLimit(path, 11); err == nil {
		t.Fatal("accepted oversize file")
	}
}
