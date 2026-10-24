package plugin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/internal/wasmtest"
	"github.com/strio-dev/bale/plugin"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func TestRunnerAdaptsClientCallback(t *testing.T) {
	runner := plugin.Runner(plugin.Options{})
	declaration, err := runner(context.Background(), bale.PluginRequest{
		ModulePath: moduleFile(t, wasmtest.Command(`{"schemaVersion":1,"context":"callback","dependencies":[]}`, 0)),
		Input:      json.RawMessage(`{}`),
	})
	if err != nil || declaration.Context != "callback" {
		t.Fatalf("callback: %+v %v", declaration, err)
	}
}

func registryPlugin(t *testing.T, output string, exit uint32) (*bale.Client, string, v1.Descriptor) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	packagePath := filepath.Join(t.TempDir(), "discovery-0.0.0.tgz")
	if err := os.WriteFile(packagePath, wasmtest.Package(wasmtest.Command(output, exit)), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := plugin.Pack(ctx, store, packagePath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := content.FetchAll(ctx, store, root)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	descriptors := map[string]v1.Descriptor{root.Digest.String(): root, manifest.Config.Digest.String(): manifest.Config, manifest.Layers[0].Digest.String(): manifest.Layers[0]}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead && r.Method != http.MethodGet {
			http.Error(w, "read-only source", http.StatusMethodNotAllowed)
			return
		}
		ref := path.Base(r.URL.Path)
		if ref == "v1" {
			ref = root.Digest.String()
		}
		desc, ok := descriptors[ref]
		if !ok {
			http.NotFound(w, r)
			return
		}
		body, err := content.FetchAll(r.Context(), store, desc)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", desc.MediaType)
		w.Header().Set("Docker-Content-Digest", desc.Digest.String())
		w.Header().Set("Content-Length", fmt.Sprint(desc.Size))
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	return client, host + "/plugins/producer:v1", root
}

func TestRunOCI(t *testing.T) {
	client, ref, root := registryPlugin(t, `{"schemaVersion":1,"context":"registry","dependencies":[{"ref":"example.com/app:v1"}]}`, 0)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	result, err := plugin.RunOCI(context.Background(), client, plugin.OCIRequest{Ref: ref, Input: json.RawMessage(`{}`)}, plugin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Descriptor.Digest != root.Digest || result.Declaration.Context != "registry" || len(result.Declaration.Dependencies) != 1 {
		t.Fatalf("result: %+v", result)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary plugin remained: %v %v", entries, err)
	}
}

func TestRunOCIDiscardsFailedGuestResult(t *testing.T) {
	client, ref, _ := registryPlugin(t, `{"schemaVersion":1,"dependencies":[]}`, 7)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	result, err := plugin.RunOCI(context.Background(), client, plugin.OCIRequest{Ref: ref, Input: json.RawMessage(`{}`)}, plugin.Options{})
	if err == nil || result.Descriptor.Digest != "" || result.Declaration.SchemaVersion != 0 {
		t.Fatalf("failed guest result retained: %+v %v", result, err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed plugin remained: %v %v", entries, err)
	}
}

func TestRunOCIRequiresClient(t *testing.T) {
	if result, err := plugin.RunOCI(context.Background(), nil, plugin.OCIRequest{}, plugin.Options{}); err == nil || result.Descriptor.Digest != "" {
		t.Fatalf("nil client: %+v %v", result, err)
	}
}
