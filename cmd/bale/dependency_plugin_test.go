package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/internal/wasmtest"
	"oras.land/oras-go/v2/content"
)

func pluginFiles(t *testing.T, output string, exit uint32) (module, input string) {
	t.Helper()
	dir := t.TempDir()
	module, input = filepath.Join(dir, "producer.wasm"), filepath.Join(dir, "request.json")
	if err := os.WriteFile(module, wasmtest.Command(output, exit), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	return module, input
}

func TestDependencyGenerate(t *testing.T) {
	raw := `{"schemaVersion":1,"context":"production","dependencies":[{"ref":"example.com/app:stable","follow":"none"},{"ref":"example.com/chart:v1","select":{"context":"nested"}},{"ref":"example.com/app:stable","follow":"none"}]}`
	module, input := pluginFiles(t, raw, 0)
	for _, args := range [][]string{{"dependencies", "generate", "--help"}, {"help", "dependencies", "generate"}} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 0 || !strings.Contains(out.String(), "--plugin") || errs.Len() != 0 {
			t.Fatalf("help: %d %s %s", code, &out, &errs)
		}
	}
	args := []string{"dependencies", "generate", "--plugin-file", module, "--input", input, "--plugin-cache="}
	var out, errs bytes.Buffer
	if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
		t.Fatalf("generate: %d %s %s", code, &out, &errs)
	}
	want, err := bale.DecodeDependencyDeclaration([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	got, err := bale.ParseDependencyDeclaration(out.Bytes())
	if err != nil || !reflect.DeepEqual(got, want) || strings.Contains(out.String(), "schemaVersion") {
		t.Fatalf("authoring round trip: %s: %v", &out, err)
	}
	if code := run(args, brokenWriter{}, &errs); code != 125 {
		t.Fatalf("ignored output failure: %d", code)
	}
}

func TestDependencyStdin(t *testing.T) {
	t.Run("declaration", func(t *testing.T) {
		withInput(t, "context: production\ndependencies:\n  - ref: example.com/app:stable\n")
		declaration, err := readDependencySource(t.Context(), commandOptions{file: "-"})
		if err != nil || declaration.Context != "production" || len(declaration.Dependencies) != 1 {
			t.Fatalf("stdin declaration: %+v: %v", declaration, err)
		}
	})
	t.Run("request", func(t *testing.T) {
		module, _ := pluginFiles(t, `{"schemaVersion":1,"dependencies":[{"ref":"example.com/app:stable"}]}`, 0)
		withInput(t, `{}`)
		var out, errs bytes.Buffer
		args := []string{"dependencies", "generate", "--plugin-file", module, "--input", "-", "--plugin-cache=", "--plugin-memory", "256MiB"}
		if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
			t.Fatalf("stdin request: exit %d stdout %s stderr %s", code, &out, &errs)
		}
		if declaration, err := bale.ParseDependencyDeclaration(out.Bytes()); err != nil || len(declaration.Dependencies) != 1 {
			t.Fatalf("generated YAML: %s: %v", &out, err)
		}
	})
	t.Run("request limit", func(t *testing.T) {
		withInput(t, strings.Repeat(" ", (8<<20)+1))
		_, err := readDependencySource(t.Context(), commandOptions{pluginModule: "not-executed", pluginInput: "-", pluginCacheSet: true})
		if err == nil || !strings.Contains(err.Error(), "exceeds 8388608 bytes") {
			t.Fatalf("stdin request limit: %v", err)
		}
	})
	t.Run("declaration limit", func(t *testing.T) {
		withInput(t, strings.Repeat(" ", int(bale.DependencyMaxBytes)+1))
		if _, err := readDependencySource(t.Context(), commandOptions{file: "-"}); err == nil {
			t.Fatal("accepted oversized declaration")
		}
	})
}

func TestDependencyPluginRejectsBeforeRegistry(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	for _, tc := range []struct {
		output string
		exit   uint32
	}{
		{`{"schemaVersion":1,"dependencies":[]}`, 7},
		{`{"schemaVersion":1,"dependencies":[{"ref":"implicit:v1"}]}`, 0},
		{`{"schemaVersion":1,"declaration":{"schemaVersion":1,"dependencies":[]}}`, 0},
	} {
		module, input := pluginFiles(t, tc.output, tc.exit)
		var out, errs bytes.Buffer
		args := []string{"dependencies", "attach", host + "/app:subject", "--plain-http", host, "--plugin-file", module, "--input", input, "--plugin-cache="}
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 {
			t.Fatalf("failed producer: %d %s %s", code, &out, &errs)
		}
	}
	for _, args := range [][]string{
		{"dependencies", "generate"},
		{"dependencies", "generate", "--plugin", "one"},
		{"dependencies", "generate", "--plugin", "one", "--plugin", "two"},
		{"dependencies", "generate", "--input", "request.json"},
		{"dependencies", "generate", "--format", "json"},
		{"dependencies", "generate", "--plain-http", host},
		{"dependencies", "generate", "--plugin", "one", "--input", "two", "extra"},
		{"dependencies", "attach", host + "/app:subject", "-f", "one", "--plugin", "two", "--input", "three"},
		{"dependencies", "attach", host + "/app:subject", "-f", "one", "--plugin-memory=65536"},
	} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 {
			t.Fatalf("invalid args %v: %d %s %s", args, code, &out, &errs)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid producer made %d registry requests", requests.Load())
	}
}

func TestDependencyPluginAttach(t *testing.T) {
	// Exercise the complete CLI against a local Distribution endpoint with the
	// OCI referrers API. The declared image host stays deliberately unreachable.
	subjectRaw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`)
	subject := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, subjectRaw)
	objects := map[string][]byte{subject.Digest.String(): subjectRaw}
	mediaTypes := map[string]string{subject.Digest.String(): subject.MediaType}
	var refs []v1.Descriptor
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		path := r.URL.Path
		if strings.Contains(path, "/referrers/") {
			w.Header().Set("Content-Type", v1.MediaTypeImageIndex)
			if err := json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 2, "mediaType": v1.MediaTypeImageIndex, "manifests": refs}); err != nil {
				t.Error(err)
			}
			return
		}
		if strings.Contains(path, "/blobs/uploads/") {
			if r.Method == http.MethodPost {
				w.Header().Set("Location", "/v2/app/blobs/uploads/id")
				w.WriteHeader(http.StatusAccepted)
				return
			}
			raw, _ := io.ReadAll(r.Body)
			key := r.URL.Query().Get("digest")
			objects[key] = raw
			w.Header().Set("Docker-Content-Digest", key)
			w.Header().Set("Location", path)
			w.WriteHeader(http.StatusCreated)
			return
		}
		var reference string
		for _, kind := range []string{"/manifests/", "/blobs/"} {
			if _, ref, ok := strings.Cut(path, kind); ok {
				reference = ref
				break
			}
		}
		if r.Method == http.MethodPut && strings.Contains(path, "/manifests/") {
			raw, _ := io.ReadAll(r.Body)
			d := content.NewDescriptorFromBytes(r.Header.Get("Content-Type"), raw)
			objects[d.Digest.String()], mediaTypes[d.Digest.String()] = raw, d.MediaType
			d.ArtifactType = bale.DependencyArtifactType
			refs = []v1.Descriptor{d}
			w.Header().Set("Docker-Content-Digest", d.Digest.String())
			w.Header().Set("OCI-Subject", subject.Digest.String())
			w.Header().Set("Location", path)
			w.WriteHeader(http.StatusCreated)
			return
		}
		if reference == "subject" {
			reference = subject.Digest.String()
		}
		raw, ok := objects[reference]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Docker-Content-Digest", reference)
		w.Header().Set("Content-Type", mediaTypes[reference])
		w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(raw)
		}
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	raw := `{"schemaVersion":1,"context":"test","dependencies":[{"ref":"unreachable.invalid/image:stable"}]}`
	module, input := pluginFiles(t, raw, 0)
	var out, errs bytes.Buffer
	args := []string{"dependencies", "attach", host + "/app:subject", "--plain-http", host, "--plugin-file", module, "--input", input, "--plugin-cache=", "--format=json", "-q"}
	if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
		t.Fatalf("attach: %d %s %s", code, &out, &errs)
	}
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	declarations, err := client.InspectDependencies(context.Background(), host+"/app:subject")
	if err != nil || len(declarations) != 1 || declarations[0].Declaration.Context != "test" || len(declarations[0].Declaration.Dependencies) != 1 || declarations[0].Declaration.Dependencies[0].Ref != "unreachable.invalid/image:stable" {
		t.Fatalf("published declarations %+v: %v", declarations, err)
	}
	withInput(t, "context: stdin\ndependencies:\n  - ref: unreachable.invalid/image:stable\n")
	out.Reset()
	errs.Reset()
	args = []string{"dependencies", "attach", host + "/app:subject", "--plain-http", host, "-f", "-", "-q"}
	if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
		t.Fatalf("attach stdin: exit %d stdout %s stderr %s", code, &out, &errs)
	}
	declarations, err = client.InspectDependencies(t.Context(), host+"/app:subject")
	if err != nil || len(declarations) != 1 || declarations[0].Declaration.Context != "stdin" {
		t.Fatalf("stdin declaration: %+v: %v", declarations, err)
	}
}
