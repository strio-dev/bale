package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	var out bytes.Buffer
	if _, err := parse([]string{"--help"}, &out); !errors.Is(err, flag.ErrHelp) {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--config", "x", "extra"}, {"--config", "x", "--tls-key-file", "key"}, {"--config", "x", "--plain-http", "https://bad"}} {
		if _, err := parse(args, &out); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	o, err := parse([]string{"--config", "x", "--plain-http", "localhost:5000", "--plain-http", "localhost:5001"}, &out)
	if err != nil || len(o.registry.PlainHTTPRegistries) != 2 || o.listen != "127.0.0.1:8080" {
		t.Fatal(o, err)
	}
}
func TestReadToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	for _, tc := range []struct {
		value string
		valid bool
	}{{"secret\n", true}, {"", false}, {"two words", false}} {
		if err := os.WriteFile(path, []byte(tc.value), 0600); err != nil {
			t.Fatal(err)
		}
		value, err := readToken(path)
		if (err == nil) != tc.valid {
			t.Fatal(tc, err)
		}
		if tc.valid && value != "secret" {
			t.Fatal(value)
		}
	}
}
