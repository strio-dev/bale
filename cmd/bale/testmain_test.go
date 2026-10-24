package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Every CLI test uses isolated persistent storage, including error-path tests.
func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "bale-cli-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for name, value := range map[string]string{
		"XDG_DATA_HOME":  filepath.Join(directory, "data"),
		"XDG_CACHE_HOME": filepath.Join(directory, "cache"),
	} {
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	if err := os.RemoveAll(directory); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
