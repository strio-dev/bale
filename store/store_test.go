package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/store"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
)

type fixture struct {
	store       *memory.Store
	descriptors map[digest.Digest]v1.Descriptor
	mu          sync.Mutex
	tag         v1.Descriptor
	payload     v1.Descriptor
	gets        atomic.Int64
	writes      atomic.Int64
}

func (f *fixture) put(t *testing.T, media string, raw []byte) v1.Descriptor {
	t.Helper()
	d := content.NewDescriptorFromBytes(media, raw)
	if err := f.store.Push(t.Context(), d, bytes.NewReader(raw)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		t.Fatal(err)
	}
	f.descriptors[d.Digest] = d
	return d
}
func (f *fixture) artifact(t *testing.T, value string) v1.Descriptor {
	t.Helper()
	empty := f.put(t, v1.MediaTypeEmptyJSON, []byte("{}"))
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: empty, Layers: []v1.Descriptor{f.payload}, Annotations: map[string]string{"value": value}})
	if err != nil {
		t.Fatal(err)
	}
	return f.put(t, v1.MediaTypeImageManifest, raw)
}
func source(t *testing.T) (*fixture, *httptest.Server, *bale.Client, bale.Recipe) {
	t.Helper()
	f := &fixture{store: memory.New(), descriptors: map[digest.Digest]v1.Descriptor{}}
	f.payload = f.put(t, "application/example", []byte("large shared payload"))
	f.tag = f.artifact(t, "one")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			f.writes.Add(1)
			http.Error(w, "read only", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		name := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
		f.mu.Lock()
		d := f.descriptors[digest.Digest(name)]
		if name == "stable" {
			d = f.tag
		}
		f.mu.Unlock()
		if d.Digest == "" {
			http.NotFound(w, r)
			return
		}
		raw, err := content.FetchAll(r.Context(), f.store, d)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", d.MediaType)
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		w.Header().Set("Docker-Content-Digest", d.Digest.String())
		if r.Method == http.MethodGet {
			if d.Digest == f.payload.Digest {
				f.gets.Add(1)
			}
			_, _ = w.Write(raw)
		}
	}))
	host := strings.TrimPrefix(server.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	recipe, err := bale.ParseRecipe([]byte(fmt.Sprintf("include:\n - ref: %s/app:stable\n   dependencies:\n     follow: none\n   referrers:\n     include: none\n", host)))
	if err != nil {
		t.Fatal(err)
	}
	return f, server, client, recipe
}
func local(t *testing.T, client *bale.Client) *store.Store {
	t.Helper()
	s, err := store.Open(client, store.Options{Path: filepath.Join(t.TempDir(), "store")})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestManagedCaptureHydratesSharedCASAndArchivesOffline(t *testing.T) {
	f, server, client, recipe := source(t)
	defer server.Close()
	s := local(t, client)
	first, err := s.Create(t.Context(), recipe, store.CreateOptions{Tags: []string{"release:v1"}, ExportDelivery: true})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.tag = f.artifact(t, "two")
	f.mu.Unlock()
	second, err := s.Create(t.Context(), recipe, store.CreateOptions{Tags: []string{"release:v2"}})
	if err != nil {
		t.Fatal(err)
	}
	if !first.MetadataOnly || !second.MetadataOnly || f.gets.Load() != 0 || f.writes.Load() != 0 {
		t.Fatalf("capture fetched or published payloads: %+v %+v GET=%d writes=%d", first, second, f.gets.Load(), f.writes.Load())
	}
	if first.Descriptor.Digest == second.Descriptor.Digest {
		t.Fatal("different captured manifests have same bundle digest")
	}
	if _, err := os.Stat(filepath.Join(s.Path(), "blobs", f.payload.Digest.Algorithm().String(), f.payload.Digest.Encoded())); !os.IsNotExist(err) {
		t.Fatal("image payload retained during create")
	}
	rows, err := s.List(t.Context())
	if err != nil || len(rows) != 2 {
		t.Fatalf("list: %+v %v", rows, err)
	}
	inspected, err := s.Inspect(t.Context(), "release:v1")
	if err != nil || !inspected.MetadataOnly || inspected.Descriptor.Digest != first.Descriptor.Digest {
		t.Fatalf("inspect: %+v %v", inspected, err)
	}
	delivery, err := s.ExportDelivery(t.Context(), "release:v1")
	if err != nil || delivery.Bundle != first.Descriptor.Digest {
		t.Fatalf("delivery: %+v %v", delivery, err)
	}
	plan, err := s.PlanPush(t.Context(), "release:v1", "destination.example/release:v1", bale.PushOptions{Delivery: &delivery})
	if err != nil || !plan.MetadataOnly || plan.LayoutPath != "release:v1" {
		t.Fatalf("preview: %+v %v", plan, err)
	}
	if f.gets.Load() != 0 {
		t.Fatal("metadata operations fetched payloads")
	}
	materialized, err := s.Materialize(t.Context(), "release:v1")
	if err != nil || materialized.Stats.BlobBytesDownloaded != f.payload.Size {
		t.Fatalf("materialize: %+v %v", materialized, err)
	}
	if f.gets.Load() != 1 {
		t.Fatalf("expected one shared payload download, got %d", f.gets.Load())
	}
	rows, err = s.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.MetadataOnly {
			t.Fatal("shared hydration left stale source dependence")
		}
	}
	server.Close()
	archive := filepath.Join(t.TempDir(), "release.tar")
	saved, err := s.Save(t.Context(), "release:v2", archive)
	if err != nil || saved.Descriptor.Digest != second.Descriptor.Digest || saved.LayoutPath != "" {
		t.Fatalf("save: %+v %v", saved, err)
	}
	if f.gets.Load() != 1 {
		t.Fatal("save redownloaded shared payload")
	}
	restored := local(t, client)
	loaded, err := restored.Load(t.Context(), archive, []string{"imported:v2"})
	if err != nil || loaded.Descriptor.Digest != second.Descriptor.Digest || loaded.LayoutPath != "" {
		t.Fatalf("load: %+v %v", loaded, err)
	}
	got, err := restored.Inspect(t.Context(), "imported:v2")
	if err != nil || got.MetadataOnly {
		t.Fatalf("restored incomplete: %+v %v", got, err)
	}
	diff, err := s.Diff(t.Context(), "release:v1", "release:v1")
	if err != nil || diff.Changed {
		t.Fatalf("diff: %+v %v", diff, err)
	}
	update, err := s.UpdateDelivery(t.Context(), "release:v1", "release:v2", delivery)
	if err != nil || update.Delivery == nil || update.Delivery.Bundle != second.Descriptor.Digest {
		t.Fatalf("update: %+v %v", update, err)
	}
}

func TestStoreOperationsStayLocalAndFailedCapturePublishesNothing(t *testing.T) {
	f, server, client, recipe := source(t)
	defer server.Close()
	s := local(t, client)
	if _, err := s.Create(t.Context(), recipe, store.CreateOptions{DryRun: true, Tags: []string{"preview"}}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.List(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatalf("dry run exposed root: %+v %v", rows, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Create(canceled, recipe, store.CreateOptions{Tags: []string{"canceled"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := s.Inspect(t.Context(), "registry.example/not-local:v1"); !errors.Is(err, bale.ErrNotFound) {
		t.Fatalf("local lookup did not preserve notfound: %v", err)
	}
	created, err := s.Create(t.Context(), recipe, store.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tag(t.Context(), created.Descriptor.Digest.String(), []string{"registry.example/local:v1"}); err != nil {
		t.Fatal(err)
	}
	server.Close()
	if _, err := s.Inspect(t.Context(), "registry.example/local:v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(t.Context(), created.Descriptor.Digest.String(), filepath.Join(t.TempDir(), "archive")); err == nil {
		t.Fatal("missing source accepted for incomplete bundle")
	}
	if f.gets.Load() != 0 || f.writes.Load() != 0 {
		t.Fatal("local read or capture accessed payload/wrote source")
	}
}

func TestFailedLoadDoesNotExposeTemporaryLayoutOrPublishRoot(t *testing.T) {
	f, server, client, recipe := source(t)
	defer server.Close()
	sourceStore := local(t, client)
	created, err := sourceStore.Create(t.Context(), recipe, store.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "source.tar")
	if _, err := sourceStore.Save(t.Context(), created.Descriptor.Digest.String(), archive); err != nil {
		t.Fatal(err)
	}
	destination := local(t, client)
	payloadPath := filepath.Join(destination.Path(), "blobs", f.payload.Digest.Algorithm().String(), f.payload.Digest.Encoded())
	if err := os.MkdirAll(filepath.Dir(payloadPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(payloadPath, []byte("corrupt preexisting content"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := destination.Load(t.Context(), archive, []string{"must-not-exist"})
	if err == nil || result.LayoutPath != "" {
		t.Fatalf("failed load returned temp layout: %+v %v", result, err)
	}
	rows, err := destination.List(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed import retained root: %+v %v", rows, err)
	}
	stages, err := filepath.Glob(filepath.Join(destination.Path(), ".load-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("failed import left staging directories: %v %v", stages, err)
	}
}
