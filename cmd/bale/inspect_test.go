package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
)

// A complete bundle whose source is deliberately unreachable. CLI reads must
// use its captured graph, never refresh the original selector.
func cliBundle(t *testing.T) (string, v1.Descriptor, v1.Descriptor) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := oci.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	put := func(mt string, value any) v1.Descriptor {
		raw, ok := value.([]byte)
		if !ok {
			raw, err = json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
		}
		desc := content.NewDescriptorFromBytes(mt, raw)
		if err := store.Push(ctx, desc, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		return desc
	}
	cfg := put(v1.MediaTypeEmptyJSON, []byte("{}"))
	payload := put("application/vnd.example.payload", []byte("captured artifact payload"))
	member := put(v1.MediaTypeImageManifest, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{payload}})
	policy := dependency.Policy{Follow: "none"}
	repo := "unreachable.invalid/app"
	snapshot, err := bundle.BuildSnapshot(ctx, store, bundle.Metadata{
		SchemaVersion: bundle.MetadataVersion,
		Recipe:        config.Recipe{Include: []config.Selection{{Ref: repo + ":stable", Dependencies: policy, Referrers: config.Policy{Include: "none"}}}},
		Roots:         []bundle.Root{{Input: 0, Repository: repo, Reference: "stable", Descriptor: member}},
		Origins:       []bundle.Origin{{Repository: repo, Descriptor: member}},
		Referrers:     []bundle.Association{},
		Dependencies:  []bundle.DependencyResolution{{Repository: repo, Subject: member, Policy: policy, Outcome: "disabled", Dependencies: []bundle.ResolvedDependency{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(ctx, snapshot.Descriptor, "bundle"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{snapshot.Descriptor}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, snapshot.Descriptor, payload
}

func TestInspectLayoutJSONAndTemplates(t *testing.T) {
	dir, root, _ := cliBundle(t)
	// A malformed Docker config proves local inspection doesn't read credentials.
	docker := t.TempDir()
	if err := os.WriteFile(filepath.Join(docker, "config.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", docker)
	before := layoutBytes(t, dir)
	for _, tc := range []struct {
		name     string
		args     []string
		expected string
	}{
		{"default array", []string{"inspect", "--layout", dir}, "array"},
		{"explicit json", []string{"inspect", dir, "--layout", "-fjson"}, "array"},
		{"legacy json", []string{"inspect", "--layout", dir, "--format", "{{json}}"}, "array"},
		{"template", []string{"inspect", "--layout", dir, "-f", "{{.Descriptor.Digest}}"}, root.Digest.String() + "\n"},
		{"template per operand", []string{"inspect", "--layout", dir, dir, "-f={{.Descriptor.Digest}}"}, strings.Repeat(root.Digest.String()+"\n", 2)},
		{"object json helper", []string{"inspect", "--layout", dir, "-f", "{{json .}}"}, "object"},
		{"json helper", []string{"inspect", "--layout", dir, "-f", "{{json .Metadata.Roots}}"}, "roots"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			if code := run(tc.args, &out, &errs); code != 0 || errs.Len() != 0 {
				t.Fatalf("exit %d stderr %s", code, &errs)
			}
			switch tc.expected {
			case "array":
				var results []bale.InspectResult
				if err := json.Unmarshal(out.Bytes(), &results); err != nil {
					t.Fatal(err)
				}
				if len(results) != 1 || results[0].Descriptor.Digest != root.Digest || len(results[0].Metadata.Roots) != 1 || results[0].Declarations == nil {
					t.Fatalf("lost verified inventory: %s", &out)
				}
			case "object":
				var result bale.InspectResult
				if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Descriptor.Digest != root.Digest {
					t.Fatalf("object json helper output %s: %v", &out, err)
				}
			case "roots":
				var roots []bundle.Root
				if err := json.Unmarshal(out.Bytes(), &roots); err != nil || len(roots) != 1 {
					t.Fatalf("json helper output %s: %v", &out, err)
				}
			default:
				if out.String() != tc.expected {
					t.Fatalf("output %q want %q", out.String(), tc.expected)
				}
			}
		})
	}
	if !reflect.DeepEqual(before, layoutBytes(t, dir)) {
		t.Fatal("inspection modified layout files")
	}
}

func layoutBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		result[strings.TrimPrefix(path, dir)] = string(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestInspectBatchKeepsSuccessAndReportsEveryFailure(t *testing.T) {
	dir, root, payload := cliBundle(t)
	corrupt, _, _ := cliBundle(t)
	corruptPath := filepath.Join(corrupt, "blobs", payload.Digest.Algorithm().String(), payload.Digest.Encoded())
	if err := os.Remove(corruptPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptPath, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	var out, errs bytes.Buffer
	if code := run([]string{"inspect", "--layout", missing, dir, corrupt}, &out, &errs); code != 125 {
		t.Fatalf("exit %d stderr %s", code, &errs)
	}
	var results []bale.InspectResult
	if err := json.Unmarshal(out.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Descriptor.Digest != root.Digest {
		t.Fatalf("successful inspection lost: %s", &out)
	}
	if !strings.HasPrefix(errs.String(), "Error: ") || !strings.Contains(errs.String(), missing) || !strings.Contains(errs.String(), corrupt) {
		t.Fatalf("missing individual errors: %s", &errs)
	}
	out.Reset()
	errs.Reset()
	if code := run([]string{"inspect", "--layout", missing}, &out, &errs); code != 125 || strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("all failures should yield empty array: exit %d stdout %s stderr %s", code, &out, &errs)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing layout was created: %v", err)
	}
}

func TestInspectInvalidTemplatePrecedesCredentialsAndStorage(t *testing.T) {
	docker := t.TempDir()
	if err := os.WriteFile(filepath.Join(docker, "config.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", docker)
	for _, args := range [][]string{
		{"inspect", "example.invalid/bundle:tag", "-f", "{{"},
		{"inspect", "--layout", filepath.Join(t.TempDir(), "missing"), "--format", "{{"},
	} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 || !strings.Contains(errs.String(), "invalid --format") {
			t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
		}
	}
}

func TestInspectOutputFailure(t *testing.T) {
	dir, _, _ := cliBundle(t)
	for _, format := range []string{"json", "{{.Descriptor.Digest}}", "{{.MissingField}}"} {
		var errs bytes.Buffer
		if code := run([]string{"inspect", "--layout", dir, "-f", format}, brokenWriter{}, &errs); code != 125 || !strings.HasPrefix(errs.String(), "Error: ") {
			t.Fatalf("exit %d stderr %s", code, &errs)
		}
	}
}

func TestInspectAndPullRemoteReadOnlyCapturedContent(t *testing.T) {
	dir, root, _ := cliBundle(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			t.Errorf("registry write: %s %s", r.Method, r.URL)
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/v2/bundles/manifests/") && !strings.HasPrefix(r.URL.Path, "/v2/bundles/blobs/") {
			t.Errorf("unexpected lookup: %s", r.URL)
			http.NotFound(w, r)
			return
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if name == "release" {
			name = root.Digest.String()
		}
		algorithm, encoded, ok := strings.Cut(name, ":")
		if !ok || algorithm != "sha256" || strings.ContainsAny(encoded, "./") {
			http.NotFound(w, r)
			return
		}
		raw, err := os.ReadFile(filepath.Join(dir, "blobs", algorithm, encoded))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeLayoutResponse(w, r, raw, name)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	reference := host + "/bundles:release"
	var out, errs bytes.Buffer
	if code := run([]string{"inspect", "--registry", reference, "--plain-http", host, "-f", "{{.Descriptor.Digest}}"}, &out, &errs); code != 0 || out.String() != root.Digest.String()+"\n" || errs.Len() != 0 {
		t.Fatalf("remote inspect exit %d stdout %s stderr %s", code, &out, &errs)
	}
	output := filepath.Join(t.TempDir(), "delivery.yaml")
	out.Reset()
	errs.Reset()
	if code := run([]string{"delivery", "export", "--registry", reference, "--plain-http", host, "-o", output}, &out, &errs); code != 0 || out.Len() != 0 || errs.Len() != 0 {
		t.Fatalf("remote export exit %d stdout %s stderr %s", code, &out, &errs)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := bale.DecodeDeliveryFile(raw)
	if err != nil || delivery.Bundle != root.Digest || len(delivery.Artifacts) != 1 {
		t.Fatalf("remote delivery %s: %v", raw, err)
	}
	for _, tc := range []struct {
		name        string
		flags       []string
		json, quiet bool
	}{
		{name: "terse"}, {name: "quiet", flags: []string{"-q"}, quiet: true}, {name: "json", flags: []string{"--format", "json"}, json: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			errs.Reset()
			args := append([]string{"pull", "--layout", reference, filepath.Join(t.TempDir(), "pulled"), "--plain-http", host}, tc.flags...)
			if code := run(args, &out, &errs); code != 0 {
				t.Fatalf("pull exit %d stdout %s stderr %s", code, &out, &errs)
			}
			if tc.json {
				var result bale.PullResult
				if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Descriptor.Digest != root.Digest {
					t.Fatalf("full pull report %s: %v", &out, err)
				}
			} else if out.String() != root.Digest.String()+"\n" {
				t.Fatalf("terse result polluted: %s", &out)
			}
			if tc.quiet && errs.Len() != 0 {
				t.Fatalf("quiet progress: %s", &errs)
			}
			if !tc.quiet && errs.Len() == 0 {
				t.Fatal("expected progress on stderr")
			}
		})
	}
}

func TestInspectTemplateFailureDoesNotWritePartialOutput(t *testing.T) {
	dir, _, _ := cliBundle(t)
	var out, errs bytes.Buffer
	code := run([]string{"inspect", "--layout", dir, dir, "-f", "prefix{{.MissingField}}"}, &out, &errs)
	if code != 125 || out.Len() != 0 || !strings.Contains(errs.String(), "MissingField") {
		t.Fatalf("template failure: exit %d stdout %s stderr %s", code, &out, &errs)
	}
}
