package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/internal/bundle"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
)

func cliDiffBundle(t *testing.T) (string, v1.Descriptor) {
	t.Helper()
	dir, _, _ := cliBundle(t)
	client, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := client.InspectLayout(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	inspected.Metadata.Recipe.Include[0].As = "renamed"
	store, err := oci.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := bundle.BuildSnapshot(t.Context(), store, inspected.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{snapshot.Descriptor}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, snapshot.Descriptor
}

func TestDiffLayoutsCLI(t *testing.T) {
	before, oldRoot, payload := cliBundle(t)
	after, newRoot := cliDiffBundle(t)
	// Local comparison has no need for credentials or opaque payloads.
	docker := t.TempDir()
	if err := os.WriteFile(filepath.Join(docker, "config.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", docker)
	if err := os.Remove(filepath.Join(before, "blobs", payload.Digest.Algorithm().String(), payload.Digest.Encoded())); err != nil {
		t.Fatal(err)
	}
	filesBefore, filesAfter := layoutBytes(t, before), layoutBytes(t, after)
	for _, tc := range []struct {
		name, format string
		identical    bool
	}{
		{name: "readable"}, {name: "identical", identical: true},
		{name: "JSON", format: "json"}, {name: "JSON shortcut", format: "{{json}}"},
		{name: "template", format: "{{.Changed}} {{.RecipeChanged}} {{.Before.Digest}} {{.After.Digest}}"},
		{name: "JSON helper", format: "{{json .}}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := after
			if tc.identical {
				target = before
			}
			args := []string{"diff", before, target, "--layout"}
			if tc.format != "" {
				args = append(args, "-f"+tc.format)
			}
			var out, errs bytes.Buffer
			if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
				t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
			}
			switch tc.name {
			case "readable":
				for _, want := range []string{oldRoot.Digest.String(), newRoot.Digest.String(), "Bundle changed: true", "Recipe changed: true", "Artifacts: 0 added, 0 removed", "Selector resolutions: 0 changed", "Dependency observations: 0 changed"} {
					if !strings.Contains(out.String(), want) {
						t.Fatalf("missing %q: %s", want, &out)
					}
				}
				if strings.Contains(out.String(), "identical") {
					t.Fatalf("recipe-only change called identical: %s", &out)
				}
			case "identical":
				if !strings.Contains(out.String(), "Bundle changed: false") || !strings.Contains(out.String(), "Bundles are identical.") {
					t.Fatalf("identical comparison: %s", &out)
				}
			case "template":
				want := fmt.Sprintf("true true %s %s\n", oldRoot.Digest, newRoot.Digest)
				if out.String() != want {
					t.Fatalf("template %q want %q", out.String(), want)
				}
			default:
				var result bale.DiffResult
				if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.Changed || !result.RecipeChanged || result.Before.Digest != oldRoot.Digest || result.After.Digest != newRoot.Digest || result.Artifacts == nil || result.Resolutions == nil || result.Declarations == nil {
					t.Fatalf("JSON result %s: %v", &out, err)
				}
			}
		})
	}
	if !reflect.DeepEqual(filesBefore, layoutBytes(t, before)) || !reflect.DeepEqual(filesAfter, layoutBytes(t, after)) {
		t.Fatal("comparison modified a layout")
	}
}

func TestDiffCLIRejectsInvalidArgumentsBeforeStorage(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"diff"}, "invalid arguments"},
		{[]string{"diff", "one"}, "invalid arguments"},
		{[]string{"diff", "one", "two", "three"}, "invalid arguments"},
		{[]string{"diff", "--registry", "example.invalid/bundle:a", "team/b:tag"}, "explicit registry"},
		{[]string{"diff", "--registry", "example.invalid/bundle:a", "example.invalid/bundle"}, "explicit tag"},
		{[]string{"diff", "one", "two", "--layout", "--mode", "latest"}, "unknown option"},
		{[]string{"diff", "one", "two", "--layout", "--quiet"}, "unknown option"},
		{[]string{"diff", "one", "two", "--layout", "--output-delivery", "file"}, "unknown option"},
		{[]string{"diff", "one", "two", "--layout", "-fjson", "--format=json"}, "duplicate option"},
		{[]string{"diff", "one", "two", "--layout", "-f", "{{"}, "invalid --format"},
		{[]string{"diff", "--registry", "example.invalid/bundle:a", "example.invalid/bundle:b", "-f", "{{"}, "invalid --format"},
	} {
		var out, errs bytes.Buffer
		if code := run(tc.args, &out, &errs); code != 125 || out.Len() != 0 || !strings.Contains(errs.String(), tc.want) {
			t.Fatalf("%v: exit %d stdout %s stderr %s", tc.args, code, &out, &errs)
		}
	}
}

func TestDiffCLIReportsInputAndOutputErrors(t *testing.T) {
	dir, _, _ := cliBundle(t)
	missing := filepath.Join(t.TempDir(), "missing")
	for _, args := range [][]string{
		{"diff", "--layout", missing, dir},
		{"diff", "--layout", dir, missing, "--format", "json"},
		{"diff", "--layout", dir, dir, "-f", "prefix{{.MissingField}}"},
	} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 || !strings.HasPrefix(errs.String(), "Error: ") {
			t.Fatalf("%v: exit %d stdout %s stderr %s", args, code, &out, &errs)
		}
	}
	for _, format := range []string{"", "json", "{{.Changed}}"} {
		var errs bytes.Buffer
		if code := run([]string{"diff", "--layout", dir, dir, "-f", format}, brokenWriter{}, &errs); code != 125 || !strings.Contains(errs.String(), "write failed") {
			t.Fatalf("%s: exit %d stderr %s", format, code, &errs)
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing layout was created: %v", err)
	}
}

func TestDiffCommandHelp(t *testing.T) {
	for _, args := range [][]string{{"diff", "--help"}, {"help", "diff"}} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
			t.Fatalf("exit %d stderr %s", code, &errs)
		}
		for _, want := range []string{"bale diff [options] BEFORE AFTER", "--layout", "-f, --format", "status 0"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("missing %q: %s", want, &out)
			}
		}
		if strings.Contains(out.String(), "--quiet") || strings.Contains(out.String(), "default: bundle digest") {
			t.Fatalf("transfer help leaked: %s", &out)
		}
	}
}

func TestWriteDiffShowsEveryKindAndQuotesContexts(t *testing.T) {
	old := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, []byte("old"))
	next := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, []byte("next"))
	context := "prod\n\t\x1b[31m"
	result := bale.DiffResult{
		Before: old, After: next, Changed: true,
		Artifacts: []bale.ArtifactChange{
			{Change: "removed", Repository: "registry.example.com/app", Descriptor: old},
			{Change: "added", Repository: "registry.example.com/app", Descriptor: next},
		},
		Declarations: []bale.DeclarationChange{{Subject: "registry.example.com/app:stable", Before: []bale.DeclarationState{{Policy: bale.DependencyPolicy{Follow: "none"}, Outcome: "disabled"}}, After: []bale.DeclarationState{
			{Policy: bale.DependencyPolicy{Follow: "auto", Select: &bale.DependencySelector{Context: context}}, Outcome: "selected", Descriptor: &next, Context: context},
			{Policy: bale.DependencyPolicy{Follow: "auto", Select: &bale.DependencySelector{Digest: next.Digest}}, Outcome: "selected", Descriptor: &next},
		}}},
	}
	for _, kind := range []string{"input", "dependency", "plugin"} {
		result.Resolutions = append(result.Resolutions, bale.ResolutionChange{Kind: kind, Ref: "registry.example.com/" + kind + ":stable", Before: []v1.Descriptor{old}, After: []v1.Descriptor{next}})
	}
	var out bytes.Buffer
	if err := writeDiff(&out, result); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Artifacts: 1 added, 1 removed", "Selector resolutions: 3 changed", "Dependency observations: 1 changed", "+ registry.example.com/app@" + next.Digest.String(), "- registry.example.com/app@" + old.Digest.String(), "input", "dependency", "plugin", "before: disabled, follow=none", "after: selected, follow=auto", "select-context=" + strconv.Quote(context), "context=" + strconv.Quote(context), "select-digest=" + next.Digest.String()} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
	if strings.Contains(out.String(), context) || strings.ContainsRune(out.String(), '\x1b') {
		t.Fatalf("unescaped terminal controls: %q", out.String())
	}
}

func TestDiffRegistryCLIUsesCapturedMetadata(t *testing.T) {
	dir, root, opaque := cliBundle(t)
	var mu sync.Mutex
	resolved := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) || (!strings.HasPrefix(r.URL.Path, "/v2/bundles/manifests/") && !strings.HasPrefix(r.URL.Path, "/v2/bundles/blobs/")) {
			t.Errorf("unexpected registry access: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if name == "old" || name == "new" {
			mu.Lock()
			resolved[name]++
			mu.Unlock()
			name = root.Digest.String()
		}
		if name == opaque.Digest.String() {
			t.Error("diff read opaque payload")
			http.NotFound(w, r)
			return
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
	var out, errs bytes.Buffer
	if code := run([]string{"diff", "--registry", host + "/bundles:old", host + "/bundles:new", "--plain-http", host, "-f", "{{.Changed}}"}, &out, &errs); code != 0 || out.String() != "false\n" || errs.Len() != 0 {
		t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
	}
	mu.Lock()
	defer mu.Unlock()
	if resolved["old"] != 1 || resolved["new"] != 1 {
		t.Fatalf("mutable references resolved repeatedly: %v", resolved)
	}
}
