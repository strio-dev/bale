package pluginoci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/wasmtest"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func packed(t *testing.T) (*memory.Store, v1.Descriptor, []byte) {
	t.Helper()
	module := wasmtest.Command(`{"schemaVersion":1,"dependencies":[]}`, 0)
	raw := wasmtest.Package(module)
	store := memory.New()
	desc, err := Pack(context.Background(), store, packageFile(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	return store, desc, module
}

func packageFile(t *testing.T, raw []byte) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "discovery.tgz")
	if err := os.WriteFile(name, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func putJSON(t *testing.T, store content.Storage, mediaType string, value any) v1.Descriptor {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return put(t, store, mediaType, raw)
}

func put(t *testing.T, store content.Storage, mediaType string, raw []byte) v1.Descriptor {
	t.Helper()
	desc := content.NewDescriptorFromBytes(mediaType, raw)
	if err := store.Push(context.Background(), desc, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	return desc
}

func manifest(t *testing.T, store content.ReadOnlyStorage, desc v1.Descriptor) v1.Manifest {
	t.Helper()
	raw, err := content.FetchAll(context.Background(), store, desc)
	if err != nil {
		t.Fatal(err)
	}
	var value v1.Manifest
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestPackOpenPreservesArchive(t *testing.T) {
	store, desc, module := packed(t)
	raw := wasmtest.Package(module)
	again, err := Pack(context.Background(), store, packageFile(t, raw))
	if err != nil || !content.Equal(desc, again) {
		t.Fatalf("non-deterministic pack: %v %v", again, err)
	}
	m := manifest(t, store, desc)
	if m.ArtifactType != ArtifactType || m.Config.MediaType != v1.MediaTypeEmptyJSON || len(m.Layers) != 1 || m.Layers[0].MediaType != v1.MediaTypeImageLayerGzip || m.Layers[0].Annotations[v1.AnnotationTitle] != "discovery-0.0.0.tgz" {
		t.Fatalf("unexpected profile: %#v", m)
	}
	got, err := content.FetchAll(context.Background(), store, m.Layers[0])
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("package archive changed: %v", err)
	}
	if !bytes.Contains(untar(t, got), []byte("Fixture license notice")) {
		t.Fatal("package lost its license")
	}
	extracted, err := Open(context.Background(), store, desc)
	if err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(extracted.Path)
	if err != nil || !bytes.Equal(got, module) {
		t.Fatalf("module changed: %v", err)
	}
	fileInfo, _ := os.Stat(extracted.Path)
	dirInfo, _ := os.Stat(filepath.Dir(extracted.Path))
	if fileInfo.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
		t.Fatal("temporary module is not private")
	}
	entries, err := os.ReadDir(filepath.Dir(extracted.Path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("ancillary package files extracted to host: %v %v", entries, err)
	}
	if err := extracted.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(extracted.Path); !os.IsNotExist(err) {
		t.Fatalf("module remained: %v", err)
	}
	if err := extracted.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidProfiles(t *testing.T) {
	for _, name := range []string{"artifact-type", "config-type", "config-content", "config-size", "missing-layer", "layer-type", "too-many-layers", "wrong-title", "oversized-package", "schema", "subject", "media-type", "provenance-title"} {
		t.Run(name, func(t *testing.T) {
			store, desc, _ := packed(t)
			m := manifest(t, store, desc)
			switch name {
			case "artifact-type":
				m.ArtifactType = "application/vnd.helm.plugin.v1+json"
			case "config-type":
				m.Config.MediaType = v1.MediaTypeImageConfig
			case "config-content":
				m.Config = putJSON(t, store, v1.MediaTypeEmptyJSON, map[string]string{"runtime": "native"})
			case "config-size":
				m.Config.Size = MaxMetadataBytes + 1
			case "missing-layer":
				m.Layers = nil
			case "layer-type":
				m.Layers[0].MediaType = "application/wasm"
			case "too-many-layers":
				m.Layers = append(m.Layers, m.Layers[0], m.Layers[0])
			case "wrong-title":
				m.Layers[0].Annotations[v1.AnnotationTitle] = "another-0.0.0.tgz"
			case "oversized-package":
				m.Layers[0].Size = MaxArchiveBytes + 1
			case "schema":
				m.SchemaVersion = 1
			case "subject":
				m.Subject = &desc
			case "media-type":
				m.MediaType = "application/vnd.docker.distribution.manifest.v2+json"
			case "provenance-title":
				m.Layers = append(m.Layers, m.Layers[0])
			}
			invalid := putJSON(t, store, v1.MediaTypeImageManifest, m)
			rejectOpen(t, store, invalid)
		})
	}
}

func TestOptionalProvenance(t *testing.T) {
	store, desc, _ := packed(t)
	m := manifest(t, store, desc)
	provenance := put(t, store, "application/octet-stream", []byte("untrusted provenance fixture"))
	provenance.Annotations = map[string]string{v1.AnnotationTitle: "discovery-0.0.0.tgz.prov"}
	m.Layers = append(m.Layers, provenance)
	desc = putJSON(t, store, v1.MediaTypeImageManifest, m)
	module, err := Open(context.Background(), store, desc)
	if err != nil {
		t.Fatal(err)
	}
	if err := module.Close(); err != nil {
		t.Fatal(err)
	}
	rejectOpen(t, tamperedStore{store, provenance.Digest, []byte("changed")}, desc)
}

func TestIndexSelection(t *testing.T) {
	for _, count := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			store, desc, _ := packed(t)
			index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex}
			for range count {
				child := desc
				child.Platform = &v1.Platform{Architecture: "wasm", OS: "wasip1"}
				index.Manifests = append(index.Manifests, child)
			}
			native := desc
			native.Platform = &v1.Platform{Architecture: "amd64", OS: "linux"}
			index.Manifests = append(index.Manifests, native)
			root := putJSON(t, store, v1.MediaTypeImageIndex, index)
			module, err := Open(context.Background(), store, root)
			if count == 1 {
				if err != nil {
					t.Fatal(err)
				}
				if err := module.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				_ = module.Close()
				t.Fatal("accepted absent or ambiguous platform")
			}
		})
	}
}

func TestIndexRejectsNestedManifest(t *testing.T) {
	store, desc, _ := packed(t)
	desc.Platform = &v1.Platform{Architecture: "wasm", OS: "wasip1"}
	desc.MediaType = v1.MediaTypeImageIndex
	index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{desc}}
	rejectOpen(t, store, putJSON(t, store, v1.MediaTypeImageIndex, index))
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

func rejectOpen(t *testing.T, store content.ReadOnlyStorage, desc v1.Descriptor) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	module, err := Open(context.Background(), store, desc)
	if err == nil {
		_ = module.Close()
		t.Fatal("accepted invalid plugin")
	}
	entries, readErr := os.ReadDir(tmp)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("temporary module leaked: %v %v", entries, readErr)
	}
}

func TestIntegrityAndCleanup(t *testing.T) {
	store, desc, _ := packed(t)
	m := manifest(t, store, desc)
	m.Config.Data = nil
	desc = putJSON(t, store, v1.MediaTypeImageManifest, m)
	for _, target := range []v1.Descriptor{desc, m.Config, m.Layers[0]} {
		raw, err := content.FetchAll(context.Background(), store, target)
		if err != nil {
			t.Fatal(err)
		}
		for _, changed := range [][]byte{raw[:len(raw)-1], append(append([]byte(nil), raw...), 0), bytes.Repeat([]byte{'x'}, len(raw))} {
			rejectOpen(t, tamperedStore{store, target.Digest, changed}, desc)
		}
	}
	for _, bad := range []v1.Descriptor{
		{MediaType: v1.MediaTypeImageManifest, Digest: desc.Digest, Size: MaxMetadataBytes + 1},
		{MediaType: v1.MediaTypeImageManifest, Digest: "bad", Size: 1},
		{MediaType: v1.MediaTypeImageManifest, Digest: desc.Digest, Size: -1},
		{MediaType: v1.MediaTypeImageManifest, Digest: desc.Digest, Size: desc.Size - 1},
	} {
		rejectOpen(t, store, bad)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, store, desc); err == nil {
		t.Fatal("ignored cancellation")
	}
}

type entry struct {
	name     string
	data     []byte
	kind     byte
	mode     int64
	declared int64
}

func fixtureEntries(t *testing.T) []entry {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(wasmtest.Package(wasmtest.Command("{}", 0))))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	var result []entry
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, entry{name: h.Name, data: raw, kind: h.Typeflag, mode: h.Mode})
	}
	return result
}

func archive(t *testing.T, entries []entry) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		kind := e.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		size := int64(len(e.data))
		if e.declared != 0 {
			size = e.declared
		}
		h := &tar.Header{Name: e.name, Mode: mode, Typeflag: kind, Size: size}
		if kind == tar.TypeSymlink || kind == tar.TypeLink {
			h.Linkname = "/outside"
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if len(e.data) > 0 {
			if _, err := tw.Write(e.data); err != nil {
				t.Fatal(err)
			}
		}
		if e.declared != 0 {
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			return output.Bytes()
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func uncheckedArtifact(t *testing.T, raw []byte) (*memory.Store, v1.Descriptor) {
	t.Helper()
	store := memory.New()
	blob := put(t, store, v1.MediaTypeImageLayerGzip, raw)
	blob.Annotations = map[string]string{v1.AnnotationTitle: "discovery-0.0.0.tgz"}
	desc, err := oras.PackManifest(context.Background(), store, oras.PackManifestVersion1_1, ArtifactType, oras.PackManifestOptions{Layers: []v1.Descriptor{blob}})
	if err != nil {
		t.Fatal(err)
	}
	return store, desc
}

func TestArchiveBoundaries(t *testing.T) {
	for _, name := range []string{"absolute", "traversal", "backslash", "noncanonical", "duplicate", "normalized-duplicate", "second-root", "root-file", "symlink", "hardlink", "device", "setuid", "missing-module", "nested-module", "component", "oversized-module", "oversized-metadata", "oversized-expanded", "too-many-files"} {
		t.Run(name, func(t *testing.T) {
			entries := fixtureEntries(t)
			switch name {
			case "absolute":
				entries[2].name = "/outside"
			case "traversal":
				entries[2].name = "discovery/../outside"
			case "backslash":
				entries[2].name = `discovery\outside`
			case "noncanonical":
				entries[2].name = "discovery//outside"
			case "duplicate":
				entries = append(entries, entries[0])
			case "normalized-duplicate":
				other := entries[0]
				other.name = "./" + other.name
				entries = append(entries, other)
			case "second-root":
				entries[2].name = "other/LICENSE"
			case "root-file":
				entries[2].name = "discovery"
			case "symlink":
				entries[2].kind = tar.TypeSymlink
				entries[2].data = nil
			case "hardlink":
				entries[2].kind = tar.TypeLink
				entries[2].data = nil
			case "device":
				entries[2].kind = tar.TypeChar
				entries[2].data = nil
			case "setuid":
				entries[2].mode = 0o4644
			case "missing-module":
				entries = append(entries[:1], entries[2:]...)
			case "nested-module":
				entries[1].name = "discovery/other/plugin.wasm"
			case "component":
				entries[1].data = []byte{0, 97, 115, 109, 13, 0, 1, 0}
			case "oversized-module":
				entries[1].declared = MaxModuleBytes + 1
				entries[1].data = nil
			case "oversized-metadata":
				entries[0].declared = MaxMetadataBytes + 1
				entries[0].data = nil
			case "oversized-expanded":
				entries[2].declared = MaxArchiveBytes + 1
				entries[2].data = nil
			case "too-many-files":
				for i := len(entries); i <= maxArchiveEntries; i++ {
					entries = append(entries, entry{name: fmt.Sprintf("discovery/notices/%d", i)})
				}
			}
			raw := archive(t, entries)
			if _, err := Pack(context.Background(), memory.New(), packageFile(t, raw)); err == nil {
				t.Fatal("published invalid archive")
			}
			store, desc := uncheckedArtifact(t, raw)
			rejectOpen(t, store, desc)
		})
	}
}

func TestMetadataContract(t *testing.T) {
	for _, name := range []string{"unknown", "duplicate", "alias", "multidoc", "api", "type", "runtime", "name", "version", "numeric-version", "wrong-root", "missing"} {
		t.Run(name, func(t *testing.T) {
			entries := fixtureEntries(t)
			raw := string(entries[0].data)
			switch name {
			case "unknown":
				raw += "config: {}\n"
			case "duplicate":
				raw += "name: discovery\n"
			case "alias":
				raw = strings.Replace(raw, "name: discovery", "name: &name discovery\nextra: *name", 1)
			case "multidoc":
				raw += "---\nname: other\n"
			case "api":
				raw = strings.Replace(raw, "apiVersion: v1", "apiVersion: v2", 1)
			case "type":
				raw = strings.Replace(raw, "type: dependency/v1", "type: cli/v1", 1)
			case "runtime":
				raw = strings.Replace(raw, "runtime: wasi/preview1", "runtime: subprocess", 1)
			case "name":
				raw = strings.Replace(raw, "name: discovery", "name: ../outside", 1)
			case "version":
				raw = strings.Replace(raw, "version: 0.0.0", "version: 01.0.0", 1)
			case "numeric-version":
				raw = strings.Replace(raw, "version: 0.0.0", "version: 1", 1)
			case "wrong-root":
				raw = strings.Replace(raw, "name: discovery", "name: other", 1)
			case "missing":
				raw = strings.Replace(raw, "runtime: wasi/preview1\n", "", 1)
			}
			entries[0].data = []byte(raw)
			store, desc := uncheckedArtifact(t, archive(t, entries))
			rejectOpen(t, store, desc)
		})
	}
	for _, version := range []string{"0.0.0", "1.2.3", "1.2.3-rc.1+build.001", "1.2.3+packaging"} {
		if !validVersion(version) {
			t.Fatalf("rejected SemVer %s", version)
		}
	}
	for _, version := range []string{"v1.2.3", "1.2", "01.2.3", "1.02.3", "1.2.3-01", "1.2.3-", "1.2.3+", "1.2.3+a+b", "1.2.3-a..b"} {
		if validVersion(version) {
			t.Fatalf("accepted non-SemVer %s", version)
		}
	}
}

func untar(t *testing.T, raw []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	value, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func compress(t *testing.T, raw []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestCompleteArchiveTrailers(t *testing.T) {
	raw := wasmtest.Package(wasmtest.Command("{}", 0))
	plain := untar(t, raw)
	for _, bad := range [][]byte{
		raw[:len(raw)-1], append(append([]byte(nil), raw...), 0), append(append([]byte(nil), raw...), raw...),
		compress(t, plain[:len(plain)-512]), compress(t, append(append([]byte(nil), plain...), []byte("hidden data")...)),
	} {
		store, desc := uncheckedArtifact(t, bad)
		rejectOpen(t, store, desc)
	}
	// Ordinary GNU tar directory entries and block padding are accepted.
	entries := append([]entry{{name: "discovery/", kind: tar.TypeDir}}, fixtureEntries(t)...)
	if _, err := Pack(context.Background(), memory.New(), packageFile(t, archive(t, entries))); err != nil {
		t.Fatal(err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestExpandedTrailerLimit(t *testing.T) {
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	plain := untar(t, wasmtest.Package(wasmtest.Command("{}", 0)))
	if _, err := gz.Write(plain); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(gz, zeroReader{}, MaxArchiveBytes); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Pack(context.Background(), memory.New(), packageFile(t, output.Bytes())); err == nil {
		t.Fatal("accepted gzip expansion beyond archive bound")
	}
}

func TestPackRejectsRawModuleAndDirectory(t *testing.T) {
	for _, raw := range [][]byte{wasmtest.Command("{}", 0), []byte("native cache"), {0, 97, 115, 109, 13, 0, 1, 0}} {
		if _, err := Pack(context.Background(), memory.New(), packageFile(t, raw)); err == nil {
			t.Fatal("accepted raw module or native source")
		}
	}
	if _, err := Pack(context.Background(), memory.New(), t.TempDir()); err == nil {
		t.Fatal("accepted directory")
	}
}

type changingStore struct {
	content.Storage
	path string
}

func (s changingStore) Push(ctx context.Context, desc v1.Descriptor, reader io.Reader) error {
	if desc.MediaType == v1.MediaTypeImageLayerGzip {
		if err := os.WriteFile(s.path, []byte("changed after first-pass validation"), 0o600); err != nil {
			return err
		}
	}
	return s.Storage.Push(ctx, desc, reader)
}
func TestPackDetectsSourceChange(t *testing.T) {
	name := packageFile(t, wasmtest.Package(wasmtest.Command("{}", 0)))
	if _, err := Pack(context.Background(), changingStore{memory.New(), name}, name); err == nil {
		t.Fatal("published changed package")
	}
}

func inlineJSON(t *testing.T, mediaType string, value any) v1.Descriptor {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	desc := content.NewDescriptorFromBytes(mediaType, raw)
	desc.Data = raw
	return desc
}
func inlinePlugin(t *testing.T, corrupt string) (v1.Descriptor, []byte) {
	t.Helper()
	module := wasmtest.Command(`{"schemaVersion":1,"dependencies":[]}`, 0)
	raw := wasmtest.Package(module)
	blob := content.NewDescriptorFromBytes(v1.MediaTypeImageLayerGzip, raw)
	blob.Data = append([]byte(nil), raw...)
	blob.Annotations = map[string]string{v1.AnnotationTitle: "discovery-0.0.0.tgz"}
	switch corrupt {
	case "archive digest":
		blob.Data[0] = 1
	case "archive truncated":
		blob.Data = blob.Data[:len(blob.Data)-1]
	case "archive trailing":
		blob.Data = append(blob.Data, 0)
	}
	config := inlineJSON(t, v1.MediaTypeEmptyJSON, map[string]any{})
	if corrupt == "config" {
		config.Data = append(config.Data, ' ')
	}
	manifest := inlineJSON(t, v1.MediaTypeImageManifest, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: ArtifactType, Config: config, Layers: []v1.Descriptor{blob}})
	if corrupt == "manifest" {
		manifest.Data[0] = '!'
	}
	manifest.Platform = &v1.Platform{Architecture: "wasm", OS: "wasip1"}
	index := inlineJSON(t, v1.MediaTypeImageIndex, v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{manifest}})
	if corrupt == "index" {
		index.Data = index.Data[:len(index.Data)-1]
	}
	return index, module
}

type noFetchStore struct{ content.ReadOnlyStorage }

func (noFetchStore) Fetch(context.Context, v1.Descriptor) (io.ReadCloser, error) {
	return nil, errors.New("inline descriptor unexpectedly fetched")
}
func TestInlineDescriptorVerification(t *testing.T) {
	store := noFetchStore{memory.New()}
	desc, want := inlinePlugin(t, "")
	module, err := Open(context.Background(), store, desc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(module.Path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("inline module changed")
	}
	if err := module.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index", "manifest", "config", "archive digest", "archive truncated", "archive trailing"} {
		bad, _ := inlinePlugin(t, name)
		rejectOpen(t, store, bad)
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
				m := inlineJSON(t, v1.MediaTypeImageManifest, fields)
				if indexed {
					m.Platform = &v1.Platform{Architecture: "wasm", OS: "wasip1"}
					index.Manifests = []v1.Descriptor{m}
					raw, err := json.Marshal(index)
					if err != nil {
						t.Fatal(err)
					}
					fields = nil
					if err := json.Unmarshal(raw, &fields); err != nil {
						t.Fatal(err)
					}
					delete(fields, "mediaType")
					if value != "omitted" {
						fields["mediaType"] = json.RawMessage(value)
					}
					root = inlineJSON(t, v1.MediaTypeImageIndex, fields)
				} else {
					root = m
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
					t.Fatal("accepted invalid envelope mediaType")
				}
			})
		}
	}
}

type postStreamErrorReader struct {
	*bytes.Reader
	err error
}

func (r postStreamErrorReader) Read(buffer []byte) (int, error) {
	n, err := r.Reader.Read(buffer)
	if errors.Is(err, io.EOF) {
		return n, r.err
	}
	return n, err
}

func TestArchivePreservesPostStreamError(t *testing.T) {
	for _, cause := range []error{errors.New("post-stream I/O failure"), context.Canceled} {
		reader := postStreamErrorReader{
			Reader: bytes.NewReader(wasmtest.Package(wasmtest.Command("{}", 0))),
			err:    cause,
		}
		if _, err := readArchive(context.Background(), reader, io.Discard); !errors.Is(err, cause) {
			t.Fatalf("lost post-stream error %v: %v", cause, err)
		}
	}
}
