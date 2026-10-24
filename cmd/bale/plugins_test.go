package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/internal/wasmtest"
	"oras.land/oras-go/v2/content"
)

func TestOCIPluginCLI(t *testing.T) {
	// One repository-scoped Distribution endpoint exercises publication, OCI
	// plugin selection, automatic create and the latest execution boundary.
	type object struct {
		descriptor v1.Descriptor
		raw        []byte
	}
	objects := map[string]object{}
	writes := map[string]int{}
	var mu sync.Mutex
	put := func(repo, tag, mediaType string, raw []byte) v1.Descriptor {
		d := content.NewDescriptorFromBytes(mediaType, raw)
		objects[repo+"@"+d.Digest.String()] = object{d, raw}
		if tag != "" {
			objects[repo+"@"+tag] = object{d, raw}
		}
		return d
	}
	cfg := put("subject", "", v1.MediaTypeEmptyJSON, []byte("{}"))
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{}})
	if err != nil {
		t.Fatal(err)
	}
	put("subject", "v1", v1.MediaTypeImageManifest, raw)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/v2/")
		if repo, subject, ok := strings.Cut(p, "/referrers/"); ok {
			refs := []v1.Descriptor{}
			for key, obj := range objects {
				if key != repo+"@"+obj.descriptor.Digest.String() || obj.descriptor.MediaType != v1.MediaTypeImageManifest {
					continue
				}
				var manifest v1.Manifest
				if json.Unmarshal(obj.raw, &manifest) == nil && manifest.Subject != nil && manifest.Subject.Digest.String() == subject {
					d := obj.descriptor
					d.ArtifactType = manifest.ArtifactType
					refs = append(refs, d)
				}
			}
			w.Header().Set("Content-Type", v1.MediaTypeImageIndex)
			if err := json.NewEncoder(w).Encode(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: refs}); err != nil {
				t.Error(err)
			}
			return
		}
		if repo, _, ok := strings.Cut(p, "/blobs/uploads/"); ok {
			writes[repo]++
			w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/id")
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			payload, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			d := put(repo, "", "application/octet-stream", payload)
			if r.URL.Query().Get("digest") != d.Digest.String() {
				t.Error("upload digest mismatch")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Docker-Content-Digest", d.Digest.String())
			w.WriteHeader(http.StatusCreated)
			return
		}
		repo, reference, ok := strings.Cut(p, "/manifests/")
		if !ok {
			repo, reference, ok = strings.Cut(p, "/blobs/")
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPut {
			writes[repo]++
			payload, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			d := put(repo, reference, r.Header.Get("Content-Type"), payload)
			w.Header().Set("Docker-Content-Digest", d.Digest.String())
			w.Header().Set("Location", r.URL.Path)
			var manifest v1.Manifest
			if json.Unmarshal(payload, &manifest) == nil && manifest.Subject != nil {
				w.Header().Set("OCI-Subject", manifest.Subject.Digest.String())
			}
			w.WriteHeader(http.StatusCreated)
			return
		}
		obj, ok := objects[repo+"@"+reference]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Docker-Content-Digest", obj.descriptor.Digest.String())
		w.Header().Set("Content-Type", obj.descriptor.MediaType)
		w.Header().Set("Content-Length", fmt.Sprint(len(obj.raw)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(obj.raw)
		}
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	invoke := func(args ...string) []byte {
		t.Helper()
		var out, errs bytes.Buffer
		args = append(args, "--plain-http", host)
		if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
			t.Fatalf("%v: code %d, stdout %s, stderr %s", args, code, &out, &errs)
		}
		return out.Bytes()
	}
	module, input := pluginFiles(t, `{"schemaVersion":1,"context":"configured","dependencies":[]}`, 0)
	pluginRef := host + "/producer:dev"
	var rejectedOut, rejectedErr bytes.Buffer
	if code := run([]string{"plugins", "push", "-f", module, pluginRef, "-q", "--plain-http", host}, &rejectedOut, &rejectedErr); code != 125 || rejectedOut.Len() != 0 || rejectedErr.Len() == 0 {
		t.Fatalf("raw module publication accepted: code %d stdout %s stderr %s", code, &rejectedOut, &rejectedErr)
	}
	mu.Lock()
	rawModuleWrites := writes["producer"]
	mu.Unlock()
	if rawModuleWrites != 0 {
		t.Fatal("raw module rejection wrote registry content")
	}
	moduleBytes, err := os.ReadFile(module)
	if err != nil {
		t.Fatal(err)
	}
	packageBytes := wasmtest.Package(moduleBytes)
	packagePath := filepath.Join(t.TempDir(), "discovery-0.0.0.tgz")
	if err := os.WriteFile(packagePath, packageBytes, 0600); err != nil {
		t.Fatal(err)
	}
	var published pluginPushResult
	if err := json.Unmarshal(invoke("plugins", "push", "-f", packagePath, pluginRef, "-q", "--format=json"), &published); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	publishedManifest := objects["producer@"+published.Descriptor.Digest.String()]
	mu.Unlock()
	var pluginManifest v1.Manifest
	if err := json.Unmarshal(publishedManifest.raw, &pluginManifest); err != nil || pluginManifest.ArtifactType != "application/vnd.bale.plugin.v1+json" || len(pluginManifest.Layers) != 1 {
		t.Fatalf("published plugin manifest: %+v %v", pluginManifest, err)
	}
	packageLayer := pluginManifest.Layers[0]
	mu.Lock()
	publishedPackage := objects["producer@"+packageLayer.Digest.String()]
	mu.Unlock()
	if packageLayer.MediaType != v1.MediaTypeImageLayerGzip || packageLayer.Annotations[v1.AnnotationTitle] != "discovery-0.0.0.tgz" || !bytes.Equal(publishedPackage.raw, packageBytes) {
		t.Fatal("publication changed the plugin archive or its descriptor")
	}
	generated := invoke("dependencies", "generate", "--plugin", pluginRef, "--input", input, "--plugin-cache=")
	declaration, err := bale.ParseDependencyDeclaration(generated)
	if err != nil || declaration.Context != "configured" {
		t.Fatalf("registry producer output %s: %v", generated, err)
	}
	recipe := filepath.Join(t.TempDir(), "bale.yaml")
	if err := os.WriteFile(recipe, fmt.Appendf(nil, "include:\n  - ref: %s/subject:v1\n    plugin:\n      ref: %s\n      input: {}\n", host, pluginRef), 0600); err != nil {
		t.Fatal(err)
	}
	bundleRef := host + "/bundles/release:v1"
	mu.Lock()
	writesBeforePreview := 0
	for _, count := range writes {
		writesBeforePreview += count
	}
	mu.Unlock()
	for _, tc := range []struct {
		name   string
		flags  []string
		quiet  bool
		format string
	}{
		{name: "inventory"},
		{name: "quiet inventory", flags: []string{"-q"}, quiet: true},
		{name: "json", flags: []string{"--format=json", "-q"}, quiet: true, format: "json"},
		{name: "template", flags: []string{"--format={{.DryRun}} {{.Published}} {{.Descriptor.Digest}}", "-q"}, quiet: true, format: "template"},
	} {
		t.Run("dry run "+tc.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			args := []string{"create", "--registry", "-f", recipe, bundleRef, "--dry-run", "--plugin-cache=", "--plain-http", host}
			args = append(args, tc.flags...)
			if code := run(args, &out, &errs); code != 0 {
				t.Fatalf("dry run: %d %s %s", code, &out, &errs)
			}
			if tc.quiet && errs.Len() != 0 {
				t.Fatalf("quiet dry run emitted progress: %s", &errs)
			}
			if !tc.quiet {
				for _, stage := range []string{"resolving", "fetching plugin", "discovering dependencies", "capturing", "captured bundle"} {
					if !strings.Contains(errs.String(), stage) {
						t.Fatalf("missing %q progress: %s", stage, &errs)
					}
				}
			}
			switch tc.format {
			case "json":
				var result bale.CreateResult
				if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.DryRun || result.Published || len(result.Metadata.Plugins) != 1 {
					t.Fatalf("JSON preview changed: %s %v", &out, err)
				}
			case "template":
				if !strings.HasPrefix(out.String(), "true false sha256:") || strings.Contains(out.String(), "Dry run") {
					t.Fatalf("template preview changed: %s", &out)
				}
			default:
				for _, want := range []string{"Dry run: nothing published.", "include[0]", host + "/subject:v1", pluginRef, published.Descriptor.Digest.String()} {
					if !strings.Contains(out.String(), want) {
						t.Fatalf("missing %q in inventory: %s", want, &out)
					}
				}
			}
		})
	}
	mu.Lock()
	writesAfterPreview := 0
	for _, count := range writes {
		writesAfterPreview += count
	}
	mu.Unlock()
	if writesAfterPreview != writesBeforePreview {
		t.Fatalf("preview published registry content: writes %d -> %d", writesBeforePreview, writesAfterPreview)
	}
	var created bale.CreateResult
	if err := json.Unmarshal(invoke("create", "--registry", "-f", recipe, bundleRef, "-q", "--plugin-cache=", "--format=json"), &created); err != nil {
		t.Fatal(err)
	}
	if !created.Published || len(created.Metadata.Plugins) != 1 || created.Metadata.Plugins[0].Descriptor.Digest != published.Descriptor.Digest {
		t.Fatalf("configured capture lost plugin pin: %+v", created.Metadata.Plugins)
	}
	mu.Lock()
	sourceWrites := writes["subject"]
	mu.Unlock()
	if sourceWrites != 0 {
		t.Fatalf("automatic discovery wrote %d times to subject repository", sourceWrites)
	}
	layout := filepath.Join(t.TempDir(), "locked")
	invoke("pull", bundleRef, layout, "-q")
	algorithm, encoded, _ := strings.Cut(packageLayer.Digest.String(), ":")
	capturedPackage, err := os.ReadFile(filepath.Join(layout, "blobs", algorithm, encoded))
	if err != nil || !bytes.Equal(capturedPackage, packageBytes) {
		t.Fatalf("captured plugin archive changed: %v", err)
	}
	latest := filepath.Join(t.TempDir(), "latest")
	var out, errs bytes.Buffer
	if code := run([]string{"pull", bundleRef, latest, "--mode=latest", "--plain-http", host, "-q"}, &out, &errs); code != 125 || !strings.Contains(errs.String(), "plugin") || out.Len() != 0 {
		t.Fatalf("latest execution permission: %d %s %s", code, &out, &errs)
	}
	if _, err := os.Stat(latest); !os.IsNotExist(err) {
		t.Fatalf("denied latest mutated layout: %v", err)
	}
	invoke("pull", bundleRef, latest, "--mode=latest", "--allow-plugins", "--plugin-cache=", "-q")
}

func TestPluginCLIValidation(t *testing.T) {
	for _, args := range [][]string{{"plugins"}, {"help", "plugins"}, {"plugins", "push", "--help"}, {"help", "plugins", "push"}} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 || !strings.Contains(out.String(), "WASIp1") && !strings.Contains(out.String(), "plugins") {
			t.Fatalf("%v: %d %s %s", args, code, &out, &errs)
		}
		if code := run(args, brokenWriter{}, &errs); code != 125 {
			t.Fatalf("ignored help write failure: %d", code)
		}
	}
	for _, args := range [][]string{
		{"plugins", "other"}, {"plugins", "push"}, {"plugins", "push", "example.com/plugin:tag"},
		{"plugins", "push", "-f", "none", "example.com/plugin"},
		{"plugins", "push", "-f", "none", "example.com/plugin@sha256:" + strings.Repeat("a", 64)},
		{"plugins", "push", "-f", "none", "example.com/plugin:tag", "--format", "{{"},
		{"dependencies", "generate", "--plugin", "example.com/plugin:v1", "--plugin-file", "local", "--input", "none"},
		{"pull", "example.com/bundle:v1", "layout", "--allow-plugins"},
	} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 || !strings.HasPrefix(errs.String(), "Error: ") {
			t.Fatalf("invalid %v: %d %s %s", args, code, &out, &errs)
		}
	}
}
