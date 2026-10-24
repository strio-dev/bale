package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/strio-dev/bale"
)

func TestTransferPartialJSON(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	dir, root, payload := cliBundle(t)
	client, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := client.InspectLayout(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	member := inspection.Metadata.Roots[0].Descriptor
	var failPayload atomic.Bool
	failPayload.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/destination/") {
			http.Error(w, "publication denied", http.StatusForbidden)
			return
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch name {
		case "release":
			name = root.Digest.String()
		case "stable":
			name = member.Digest.String()
		}
		dgst, err := digest.Parse(name)
		if err != nil || (failPayload.Load() && strings.HasPrefix(r.URL.Path, "/v2/bundles/") && dgst == payload.Digest) {
			http.NotFound(w, r)
			return
		}
		raw, err := os.ReadFile(filepath.Join(dir, "blobs", dgst.Algorithm().String(), dgst.Encoded()))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeLayoutResponse(w, r, raw, dgst.String())
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	recipe := filepath.Join(t.TempDir(), "bale.yaml")
	if err := os.WriteFile(recipe, fmt.Appendf(nil, "include:\n  - ref: %s/app:stable\n    dependencies:\n      follow: none\n    referrers:\n      include: none\n", host), 0600); err != nil {
		t.Fatal(err)
	}
	arguments := func(command string) []string {
		var args []string
		switch command {
		case "create":
			args = []string{command, "--registry", "-f", recipe, host + "/destination:bundle"}
		case "pull":
			args = []string{command, "--layout", host + "/bundles:release", filepath.Join(t.TempDir(), "pulled")}
		case "push":
			args = []string{command, "--layout", dir, host + "/destination:bundle"}
		}
		return append(args, "--plain-http", host, "--quiet")
	}
	for _, command := range []string{"create", "pull", "push"} {
		t.Run(command, func(t *testing.T) {
			for _, format := range []string{"json", "{{json}}", "{{json.}}", "", "{{.Descriptor.Digest}}", "{{json .}}"} {
				t.Run(format, func(t *testing.T) {
					args := arguments(command)
					if format != "" {
						args = append(args, "--format", format)
					}
					var out, errs bytes.Buffer
					if code := run(args, &out, &errs); code != 125 || !strings.HasPrefix(errs.String(), "Error: ") {
						t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
					}
					if format == "" || format == "{{.Descriptor.Digest}}" || format == "{{json .}}" {
						if out.Len() != 0 {
							t.Fatalf("failure emitted success output: %s", &out)
						}
						return
					}
					switch command {
					case "create":
						var report bale.CreateResult
						if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Descriptor.Digest == "" || report.Published || len(report.Metadata.Roots) != 1 {
							t.Fatalf("partial create %s: %v", &out, err)
						}
					case "pull":
						var report bale.PullResult
						if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Descriptor.Digest != root.Digest || report.LayoutPath != "" {
							t.Fatalf("partial pull %s: %v", &out, err)
						}
					case "push":
						var report bale.PushResult
						if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Descriptor.Digest != root.Digest || len(report.Tags) != 0 || len(report.Relocations) != 0 || len(report.Destinations) != 0 {
							t.Fatalf("partial push %s: %v", &out, err)
						}
					}
				})
			}
		})
	}
	t.Run("write failure retains transfer error", func(t *testing.T) {
		var errs bytes.Buffer
		args := append(arguments("push"), "--format", "json")
		if code := run(args, partialReportFailWriter{}, &errs); code != 125 || !strings.Contains(errs.String(), "403") || !strings.Contains(errs.String(), "partial report output failed") {
			t.Fatalf("exit %d stderr %s", code, &errs)
		}
	})
	t.Run("dry run failure stays empty", func(t *testing.T) {
		var out, errs bytes.Buffer
		args := append(arguments("create"), "--format", "json", "--dry-run")
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 {
			t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
		}
	})
	t.Run("capture and resolution failures stay empty", func(t *testing.T) {
		missingRecipe := filepath.Join(t.TempDir(), "bale.yaml")
		if err := os.WriteFile(missingRecipe, fmt.Appendf(nil, "include:\n  - ref: %s/app:missing\n", host), 0600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"create", "--registry", "-f", missingRecipe, host + "/destination:bundle"},
			{"pull", "--layout", host + "/bundles:missing", filepath.Join(t.TempDir(), "pulled")},
		} {
			var out, errs bytes.Buffer
			args = append(args, "--plain-http", host, "--quiet", "--format", "json")
			if code := run(args, &out, &errs); code != 125 || out.Len() != 0 || !strings.Contains(errs.String(), "not found") {
				t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
			}
		}
	})
	t.Run("successful pull unchanged", func(t *testing.T) {
		failPayload.Store(false)
		for _, format := range []string{"json", "", "{{.Descriptor.Digest}}"} {
			var out, errs bytes.Buffer
			args := arguments("pull")
			if format != "" {
				args = append(args, "--format", format)
			}
			if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
				t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
			}
			if format == "json" {
				var report bale.PullResult
				if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Descriptor.Digest != root.Digest || report.LayoutPath == "" {
					t.Fatalf("successful pull %s: %v", &out, err)
				}
			} else if out.String() != root.Digest.String()+"\n" {
				t.Fatalf("success output changed: %s", &out)
			}
		}
	})
	t.Run("progress output failure", func(t *testing.T) {
		for _, format := range []string{"json", "", "{{.Descriptor.Digest}}"} {
			var out bytes.Buffer
			errs := &partialProgressFailWriter{}
			args := []string{"pull", "--layout", host + "/bundles:release", filepath.Join(t.TempDir(), "pulled"), "--plain-http", host}
			if format != "" {
				args = append(args, "--format", format)
			}
			if code := run(args, &out, errs); code != 125 || !strings.Contains(errs.String(), "progress output failed") {
				t.Fatalf("exit %d stdout %s stderr %s", code, &out, errs)
			}
			if format == "json" {
				var report bale.PullResult
				if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Descriptor.Digest != root.Digest || report.LayoutPath == "" {
					t.Fatalf("completed pull with progress failure %s: %v", &out, err)
				}
			} else if out.Len() != 0 {
				t.Fatalf("progress failure emitted success output: %s", &out)
			}
		}
	})
}

func TestTransferJSONWithoutResultStaysEmpty(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	recipe := filepath.Join(t.TempDir(), "bale.yaml")
	if err := os.WriteFile(recipe, []byte("include: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"push", "--layout", "missing-layout", "example.invalid/bundle:v1"},
		{"push", "--layout", "missing-layout", "example.invalid/bundle:v1", "--dry-run"},
		{"create", "--registry", "-f", recipe, "example.invalid/bundle:v1"},
		{"create", "--registry", "-f", "missing-recipe", "example.invalid/bundle:v1"},
		{"pull", "--layout", "example.invalid/bundle", "unused"},
	} {
		var out, errs bytes.Buffer
		args = append(args, "--format", "json", "--quiet")
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 || !strings.HasPrefix(errs.String(), "Error: ") {
			t.Fatalf("%v: exit %d stdout %s stderr %s", args, code, &out, &errs)
		}
	}
}

type partialReportFailWriter struct{}

func (partialReportFailWriter) Write([]byte) (int, error) {
	return 0, errors.New("partial report output failed")
}

type partialProgressFailWriter struct {
	bytes.Buffer
	failed bool
}

func (w *partialProgressFailWriter) Write(data []byte) (int, error) {
	if !w.failed {
		w.failed = true
		return 0, errors.New("progress output failed")
	}
	return w.Buffer.Write(data)
}
