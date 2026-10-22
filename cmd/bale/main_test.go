package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestCLI(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code int
		text string
	}{
		{"default", nil, 0, "Usage:"},
		{"help", []string{"--help"}, 0, "Available Commands:"},
		{"version", []string{"--version"}, 0, "bale dev"},
		{"unsupported", []string{"bundle"}, 125, "unsupported command"},
		{"extra arguments", []string{"--version", "extra"}, 125, "unsupported command"},
		{"command help", []string{"pull", "--help"}, 0, "locked|latest"},
		{"missing operands", []string{"pull"}, 125, "invalid arguments"},
		{"unknown flag", []string{"pull", "--other"}, 125, "unknown option"},
		{"duplicate flag", []string{"pull", "--mode", "locked", "--mode", "latest"}, 125, "duplicate option"},
		{"missing flag value", []string{"create", "--file"}, 125, "requires a value"},
		{"bad mode after operands", []string{"pull", "example.com/b:tag", "layout", "--mode", "newest"}, 125, "--mode must"},
		{"implicit registry", []string{"pull", "team/b:tag", "layout"}, 125, "explicit registry"},
		{"implicit tag", []string{"pull", "example.com/b", "layout"}, 125, "explicit tag"},
		{"bad HTTP scope", []string{"pull", "example.com/b:tag", "layout", "--plain-http", "http://example.com"}, 125, "invalid registry"},
		{"bad mapping", []string{"push", "layout", "example.com/b:tag", "--map", "oops"}, 125, "--map requires"},
		{"missing recipe", []string{"create", "--file", "/no/such/bale-test-file", "example.com/b:tag"}, 125, "no such file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			if got := run(tc.args, &out, &errs); got != tc.code {
				t.Fatalf("exit %d, want %d; stderr %q", got, tc.code, errs.String())
			}
			stream := &out
			if tc.code != 0 {
				stream = &errs
				if !strings.HasPrefix(errs.String(), "Error: ") {
					t.Fatalf("missing Error: prefix: %q", errs.String())
				}
				if out.Len() != 0 {
					t.Fatal("unexpected stdout")
				}
			} else if errs.Len() != 0 {
				t.Fatal("unexpected stderr")
			}
			if !strings.Contains(stream.String(), tc.text) {
				t.Fatalf("unexpected output: %q", stream.String())
			}
		})
	}
}

func TestFlagsAfterOperands(t *testing.T) {
	opts, err := parseCommand("pull", []string{"example.com/b:tag", "layout", "--mode=latest", "--plain-http", "localhost:5000", "--plain-http=localhost:5001"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.mode != "latest" || len(opts.args) != 2 || opts.args[1] != "layout" || len(opts.plainHTTP) != 2 {
		t.Fatalf("unexpected parse: %+v", opts)
	}
	opts, err = parseCommand("create", []string{"--dry-run", "example.com/b:tag", "--file", "recipe.yaml"})
	if err != nil || !opts.dryRun || opts.file != "recipe.yaml" || len(opts.args) != 1 {
		t.Fatalf("unexpected parse: %+v %v", opts, err)
	}
}

func TestMappingValidation(t *testing.T) {
	for _, maps := range [][]string{{"example.com/a=../bad"}, {"example.com/a=/absolute"}, {"example.com/a:tag=x"}, {"example.com/a=x", "example.com/a=y"}, {"team/a=x"}} {
		if _, err := parseMaps(maps); err == nil {
			t.Errorf("accepted %v", maps)
		}
	}
	maps, err := parseMaps([]string{"example.com/a=team/a", "localhost:5000/b=other"})
	if err != nil || maps["example.com/a"] != "team/a" {
		t.Fatalf("map parse: %v %v", maps, err)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
func TestOutputFailure(t *testing.T) {
	for _, args := range [][]string{nil, {"--version"}, {"bundle"}} {
		if got := run(args, brokenWriter{}, brokenWriter{}); got != 125 {
			t.Fatalf("exit %d, want 125", got)
		}
	}
	if got := run([]string{"--help"}, io.Discard, io.Discard); got != 0 {
		t.Fatalf("exit %d", got)
	}
}

func TestCommandHelpStaysSpecific(t *testing.T) {
	for _, args := range [][]string{{"inspect", "--help"}, {"help", "inspect"}} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
			t.Fatalf("exit %d stderr %s", code, &errs)
		}
		if !strings.Contains(out.String(), "bale inspect [options] BUNDLE...") || strings.Contains(out.String(), "--dry-run") || strings.Contains(out.String(), "--mode") {
			t.Fatalf("unexpected command help %s", &out)
		}
	}
}

func TestFlagAliasesAndDelimiter(t *testing.T) {
	for _, tc := range []struct {
		command      string
		args         []string
		file, format string
		operands     []string
	}{
		{"create", []string{"registry.example/b:tag", "-frecipe.yaml"}, "recipe.yaml", "", []string{"registry.example/b:tag"}},
		{"inspect", []string{"--layout", "-fjson", "--", "--format", "-odd-layout"}, "", "json", []string{"--format", "-odd-layout"}},
		{"inspect", []string{"one", "--format=json", "two", "--layout"}, "", "json", []string{"one", "two"}},
	} {
		opts, err := parseCommand(tc.command, tc.args)
		if err != nil || opts.file != tc.file || opts.format != tc.format || strings.Join(opts.args, "\x00") != strings.Join(tc.operands, "\x00") {
			t.Fatalf("%s %v: %+v %v", tc.command, tc.args, opts, err)
		}
	}
	for _, args := range [][]string{
		{"create", "-f", "one", "--file=two"},
		{"inspect", "-fjson", "--format=json"},
		{"pull", "-q", "--quiet"},
		{"inspect", "-h", "--help"},
	} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 || !strings.Contains(errs.String(), "duplicate option") {
			t.Fatalf("aliases %v: exit %d stdout %s stderr %s", args, code, &out, &errs)
		}
	}
}
