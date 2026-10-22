package pluginoci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/wasmtest"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func packed(t *testing.T) (*memory.Store, v1.Descriptor, []byte) {
	t.Helper()
	data := wasmtest.Command(`{"dependencies":[]}`, 0)
	path := filepath.Join(t.TempDir(), "plugin.wasm")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store := memory.New()
	desc, err := Pack(context.Background(), store, path)
	if err != nil {
		t.Fatal(err)
	}
	return store, desc, data
}

func TestPackOpen(t *testing.T) {
	store, desc, data := packed(t)
	path := filepath.Join(t.TempDir(), "again.wasm")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := Pack(context.Background(), store, path)
	if err != nil || !content.Equal(desc, again) {
		t.Fatalf("non-deterministic pack: %v %v", again, err)
	}
	module, err := Open(context.Background(), store, desc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(module.Path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("module bytes changed: %v", err)
	}
	fileInfo, _ := os.Stat(module.Path)
	dirInfo, _ := os.Stat(filepath.Dir(module.Path))
	if fileInfo.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
		t.Fatal("temporary module is not private")
	}
	if err := module.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(module.Path); !os.IsNotExist(err) {
		t.Fatalf("temporary module remained: %v", err)
	}
	if err := module.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidProfiles(t *testing.T) {
	for _, name := range []string{"native", "wrong-os", "missing-digests", "wrong-digest", "multiple-layers", "wrong-layer", "wrong-config", "docker", "schema", "oversized-module", "oversized-config"} {
		t.Run(name, func(t *testing.T) {
			store, desc, _ := packed(t)
			manifestData, err := content.FetchAll(context.Background(), store, desc)
			if err != nil {
				t.Fatal(err)
			}
			var manifest v1.Manifest
			if err := json.Unmarshal(manifestData, &manifest); err != nil {
				t.Fatal(err)
			}
			config := moduleConfig{Architecture: "wasm", OS: "wasip1", LayerDigests: []digest.Digest{manifest.Layers[0].Digest}}
			switch name {
			case "native":
				config.Architecture = "amd64"
			case "wrong-os":
				config.OS = "linux"
			case "missing-digests":
				config.LayerDigests = nil
			case "wrong-digest":
				config.LayerDigests = []digest.Digest{digest.FromString("wrong")}
			case "multiple-layers":
				manifest.Layers = append(manifest.Layers, manifest.Layers[0])
			case "wrong-layer":
				manifest.Layers[0].MediaType = "application/octet-stream"
			case "wrong-config":
				manifest.Config.MediaType = v1.MediaTypeImageConfig
			case "docker":
				manifest.MediaType = "application/vnd.docker.distribution.manifest.v2+json"
			case "schema":
				manifest.SchemaVersion = 1
			case "oversized-module":
				manifest.Layers[0].Size = MaxModuleBytes + 1
			}
			if name != "wrong-config" {
				manifest.Config, err = pushJSON(context.Background(), store, MediaTypeConfig, config)
				if err != nil {
					t.Fatal(err)
				}
			}
			if name == "oversized-config" {
				manifest.Config.Size = MaxMetadataBytes + 1
			}
			desc, err = pushJSON(context.Background(), store, v1.MediaTypeImageManifest, manifest)
			if err != nil {
				t.Fatal(err)
			}
			module, err := Open(context.Background(), store, desc)
			if module != nil {
				_ = module.Close()
				t.Fatal("accepted invalid profile")
			}
			if err == nil {
				t.Fatal("accepted invalid profile")
			}
		})
	}
}

func TestIndexSelection(t *testing.T) {
	for _, count := range []int{0, 1, 2} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			store, desc, data := packed(t)
			index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex}
			for i := 0; i < count; i++ {
				child := desc
				child.Platform = &v1.Platform{Architecture: "wasm", OS: "wasip1"}
				index.Manifests = append(index.Manifests, child)
			}
			nativePath := filepath.Join(t.TempDir(), "native.wasm")
			if err := os.WriteFile(nativePath, wasmtest.Command("wrong module", 0), 0600); err != nil {
				t.Fatal(err)
			}
			native, err := Pack(context.Background(), store, nativePath)
			if err != nil {
				t.Fatal(err)
			}
			native.Platform = &v1.Platform{Architecture: "amd64", OS: "linux"}
			index.Manifests = append(index.Manifests, native)
			root, err := pushJSON(context.Background(), store, v1.MediaTypeImageIndex, index)
			if err != nil {
				t.Fatal(err)
			}
			module, err := Open(context.Background(), store, root)
			if count == 1 {
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := module.Close(); err != nil {
						t.Error(err)
					}
				}()
				got, err := os.ReadFile(module.Path)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("wrong module selected: %v", err)
				}
			} else if err == nil {
				_ = module.Close()
				t.Fatal("accepted ambiguous or absent platform")
			}
		})
	}
}

type tamperedStore struct {
	content.ReadOnlyStorage
	target digest.Digest
	data   []byte
}

func (s tamperedStore) Fetch(ctx context.Context, desc v1.Descriptor) (io.ReadCloser, error) {
	if desc.Digest == s.target {
		return io.NopCloser(bytes.NewReader(s.data)), nil
	}
	return s.ReadOnlyStorage.Fetch(ctx, desc)
}

func TestVerifyAndCleanup(t *testing.T) {
	for _, name := range []string{"truncated", "trailing", "digest", "component"} {
		t.Run(name, func(t *testing.T) {
			store, desc, data := packed(t)
			var manifest v1.Manifest
			manifestData, err := content.FetchAll(context.Background(), store, desc)
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(manifestData, &manifest); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "truncated":
				data = data[:len(data)-1]
			case "trailing":
				data = append(data, 0)
			case "digest":
				data[0] = 1
			case "component":
				data[4] = 13
				manifest.Layers[0] = content.NewDescriptorFromBytes(MediaTypeModule, data)
				manifest.Config, err = pushJSON(context.Background(), store, MediaTypeConfig, moduleConfig{Architecture: "wasm", OS: "wasip1", LayerDigests: []digest.Digest{manifest.Layers[0].Digest}})
				if err != nil {
					t.Fatal(err)
				}
				desc, err = pushJSON(context.Background(), store, v1.MediaTypeImageManifest, manifest)
				if err != nil {
					t.Fatal(err)
				}
			}
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			module, err := Open(context.Background(), tamperedStore{store, manifest.Layers[0].Digest, data}, desc)
			if err == nil {
				_ = module.Close()
				t.Fatal("accepted tampered module")
			}
			entries, err := os.ReadDir(tmp)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary module leaked: %v %v", entries, err)
			}
		})
	}
}

func TestMetadataValidation(t *testing.T) {
	store, desc, _ := packed(t)
	for _, bad := range []v1.Descriptor{
		{MediaType: v1.MediaTypeImageManifest, Digest: desc.Digest, Size: MaxMetadataBytes + 1},
		{MediaType: v1.MediaTypeImageManifest, Digest: "bad", Size: 1},
		{MediaType: v1.MediaTypeImageManifest, Digest: desc.Digest, Size: -1},
		{MediaType: v1.MediaTypeImageManifest, Digest: desc.Digest, Size: desc.Size - 1},
	} {
		if module, err := Open(context.Background(), store, bad); err == nil {
			_ = module.Close()
			t.Fatalf("accepted bad metadata descriptor: %v", bad)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, store, desc); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestPackRejectsUnsupportedSource(t *testing.T) {
	for _, data := range [][]byte{[]byte("native cache"), {0, 97, 115, 109, 13, 0, 1, 0}, coreHeader[:4]} {
		path := filepath.Join(t.TempDir(), "bad.wasm")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Pack(context.Background(), memory.New(), path); err == nil {
			t.Fatal("accepted unsupported source")
		}
	}
	if _, err := Pack(context.Background(), memory.New(), t.TempDir()); err == nil {
		t.Fatal("accepted directory")
	}
}

func TestMetadataIntegrity(t *testing.T) {
	store, desc, _ := packed(t)
	data, err := content.FetchAll(context.Background(), store, desc)
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range [][]byte{data[:len(data)-1], append(append([]byte(nil), data...), ' '), bytes.Repeat([]byte{' '}, len(data))} {
		if module, err := Open(context.Background(), tamperedStore{store, desc.Digest, changed}, desc); err == nil {
			_ = module.Close()
			t.Fatal("accepted unverified metadata")
		}
	}
}

func TestIndexRejectsUnsupportedChild(t *testing.T) {
	store, desc, _ := packed(t)
	desc.Platform = &v1.Platform{Architecture: "wasm", OS: "wasip1"}
	desc.MediaType = v1.MediaTypeImageIndex
	index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{desc}}
	root, err := pushJSON(context.Background(), store, v1.MediaTypeImageIndex, index)
	if err != nil {
		t.Fatal(err)
	}
	if module, err := Open(context.Background(), store, root); err == nil {
		_ = module.Close()
		t.Fatal("accepted nested index")
	}
}

func TestConfigMetadataAndArtifactTypeAllowed(t *testing.T) {
	store, desc, _ := packed(t)
	data, err := content.FetchAll(context.Background(), store, desc)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Config, err = pushJSON(context.Background(), store, MediaTypeConfig, map[string]any{
		"architecture": "wasm", "os": "wasip1", "layerDigests": []digest.Digest{manifest.Layers[0].Digest},
		"author": "an ecosystem producer", "config": map[string]any{"producer": "another tool"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest.ArtifactType = "application/vnd.wasm.content.layer.v1+wasm"
	desc, err = pushJSON(context.Background(), store, v1.MediaTypeImageManifest, manifest)
	if err != nil {
		t.Fatal(err)
	}
	module, err := Open(context.Background(), store, desc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := module.Close(); err != nil {
			t.Error(err)
		}
	}()
}

type changingStore struct {
	content.Storage
	path string
}

func (s changingStore) Push(ctx context.Context, desc v1.Descriptor, reader io.Reader) error {
	if desc.MediaType == MediaTypeModule {
		if err := os.WriteFile(s.path, []byte("source changed after first-pass hash"), 0o600); err != nil {
			return err
		}
	}
	return s.Storage.Push(ctx, desc, reader)
}

func TestPackDetectsSourceChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "module.wasm")
	if err := os.WriteFile(path, wasmtest.Command("{}", 0), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Pack(context.Background(), changingStore{memory.New(), path}, path); err == nil {
		t.Fatal("accepted source modified after hashing")
	}
}

func inlineJSON(t *testing.T, mediaType string, value any) v1.Descriptor {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	desc := content.NewDescriptorFromBytes(mediaType, data)
	desc.Data = data
	return desc
}

func inlinePlugin(t *testing.T, corrupt string) (v1.Descriptor, []byte) {
	t.Helper()
	data := wasmtest.Command(`{"schemaVersion":1,"dependencies":[]}`, 0)
	blob := content.NewDescriptorFromBytes(MediaTypeModule, data)
	blob.Data = append([]byte(nil), data...)
	switch corrupt {
	case "module digest":
		blob.Data[0] = 1
	case "module truncated":
		blob.Data = blob.Data[:len(blob.Data)-1]
	case "module trailing":
		blob.Data = append(blob.Data, 0)
	}
	config := inlineJSON(t, MediaTypeConfig, moduleConfig{Architecture: "wasm", OS: "wasip1", LayerDigests: []digest.Digest{blob.Digest}})
	if corrupt == "config" {
		config.Data = append(config.Data, ' ')
	}
	manifest := inlineJSON(t, v1.MediaTypeImageManifest, v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest,
		Config: config, Layers: []v1.Descriptor{blob},
	})
	if corrupt == "manifest" {
		manifest.Data[0] = '!'
	}
	manifest.Platform = &v1.Platform{Architecture: "wasm", OS: "wasip1"}
	index := inlineJSON(t, v1.MediaTypeImageIndex, v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{manifest},
	})
	if corrupt == "index" {
		index.Data = index.Data[:len(index.Data)-1]
	}
	return index, data
}

type noFetchStore struct{ content.ReadOnlyStorage }

func (s noFetchStore) Fetch(context.Context, v1.Descriptor) (io.ReadCloser, error) {
	return nil, errors.New("inline descriptor unexpectedly fetched from backing storage")
}

func TestInlinePluginWithoutBackingObjects(t *testing.T) {
	desc, data := inlinePlugin(t, "")
	module, err := Open(context.Background(), noFetchStore{memory.New()}, desc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := module.Close(); err != nil {
			t.Error(err)
		}
	}()
	got, err := os.ReadFile(module.Path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("inline module bytes changed: %v", err)
	}
}

func TestInlinePluginIntegrityAndCleanup(t *testing.T) {
	for _, corrupt := range []string{"index", "manifest", "config", "module digest", "module truncated", "module trailing"} {
		t.Run(corrupt, func(t *testing.T) {
			desc, _ := inlinePlugin(t, corrupt)
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			if module, err := Open(context.Background(), noFetchStore{memory.New()}, desc); err == nil {
				_ = module.Close()
				t.Fatal("accepted corrupt inline data")
			}
			entries, err := os.ReadDir(tmp)
			if err != nil || len(entries) != 0 {
				t.Fatalf("inline module leaked: %v %v", entries, err)
			}
		})
	}
}

func TestOptionalEnvelopeMediaTypes(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		for _, value := range []string{"omitted", `""`, `null`, `12`, `false`, `{}`, `[]`, `"application/unsupported"`} {
			t.Run(fmt.Sprintf("index=%t/type=%s", indexed, value), func(t *testing.T) {
				root, _ := inlinePlugin(t, "")
				var index v1.Index
				if err := json.Unmarshal(root.Data, &index); err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(index.Manifests[0].Data, &fields); err != nil {
					t.Fatal(err)
				}
				delete(fields, "mediaType")
				if !indexed && value != "omitted" {
					fields["mediaType"] = json.RawMessage(value)
				}
				manifest := inlineJSON(t, v1.MediaTypeImageManifest, fields)
				if indexed {
					manifest.Platform = &v1.Platform{Architecture: "wasm", OS: "wasip1"}
					index.Manifests = []v1.Descriptor{manifest}
					indexData, err := json.Marshal(index)
					if err != nil {
						t.Fatal(err)
					}
					fields = nil
					if err := json.Unmarshal(indexData, &fields); err != nil {
						t.Fatal(err)
					}
					delete(fields, "mediaType")
					if value != "omitted" {
						fields["mediaType"] = json.RawMessage(value)
					}
					root = inlineJSON(t, v1.MediaTypeImageIndex, fields)
				} else {
					root = manifest
				}
				module, err := Open(context.Background(), noFetchStore{memory.New()}, root)
				if value == "omitted" {
					if err != nil {
						t.Fatal(err)
					}
					if err := module.Close(); err != nil {
						t.Fatal(err)
					}
				} else if err == nil {
					_ = module.Close()
					t.Fatal("accepted conflicting or non-string mediaType")
				}
			})
		}
	}
}
