package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/content/oci"
)

// A manifest digest is deliberately unavailable through the blob endpoint.
// Registries need not expose the same bytes through both APIs.
func manifestLayerRegistry(t *testing.T) (*readRegistry, string, *Service, *atomic.Int64) {
	t.Helper()
	r := &readRegistry{store: &testStore{metadataStore: newMetadataStore(), descriptors: map[string]v1.Descriptor{}}, tags: map[string]v1.Descriptor{}, refs: map[string][]v1.Descriptor{}, headTags: map[string]int{}}
	rejected := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if i := strings.Index(req.URL.Path, "/blobs/"); i >= 0 {
			d, ok := r.store.descriptors[req.URL.Path[i+len("/blobs/"):]]
			if ok && bundle.IsEnvelope(d.MediaType) {
				rejected.Add(1)
				w.WriteHeader(http.StatusNotFound)
				return
			}
		}
		r.serve(w, req)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	service, err := NewService(Options{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	return r, host + "/test/artifacts", service, rejected
}

func manifestLayerFixture(t *testing.T, r *readRegistry) (v1.Descriptor, []v1.Descriptor) {
	t.Helper()
	amd := object(t, r.store, "amd64 image payload")
	arm := object(t, r.store, "arm64 image payload")
	amd.Platform = &v1.Platform{OS: "linux", Architecture: "amd64"}
	arm.Platform = &v1.Platform{OS: "linux", Architecture: "arm64"}
	raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{amd, arm}})
	if err != nil {
		t.Fatal(err)
	}
	index := put(t, r.store, v1.MediaTypeImageIndex, raw)
	cfg := put(t, r.store, "application/example.cache.config", []byte("{}"))
	raw, err = json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: "application/example.cache", Config: cfg, Layers: []v1.Descriptor{index}})
	if err != nil {
		t.Fatal(err)
	}
	root := put(t, r.store, v1.MediaTypeImageManifest, raw)
	r.tags["stable"] = root
	return root, []v1.Descriptor{root, index, amd, arm}
}

func TestCaptureManifestLayersDiscoversNestedReferrers(t *testing.T) {
	r, repository, service, rejected := manifestLayerRegistry(t)
	_, envelopes := manifestLayerFixture(t, r)
	attachment := refIndex(t, r, envelopes[3], []v1.Descriptor{})
	graph, snapshot, err := service.capture(ctx, config.Recipe{Include: []config.Selection{{Ref: repository + ":stable", Referrers: policy("all")}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Metadata.Origins) != len(envelopes)+1 {
		t.Fatalf("lost manifest layer origins: %+v", snapshot.Metadata.Origins)
	}
	if len(snapshot.Metadata.Referrers) != 1 || snapshot.Metadata.Referrers[0].Descriptor.Digest != attachment.Digest {
		t.Fatalf("lost layer descendant referrer: %+v", snapshot.Metadata.Referrers)
	}
	if r.blobReads != 0 || graph.stats.BlobBytesDownloaded != 0 {
		t.Fatal("capture downloaded opaque payloads")
	}
	if rejected.Load() != 0 {
		t.Fatal("capture requested a manifest through the blob endpoint")
	}
}

func TestPullManifestLayersPreservesFullOfflineClosure(t *testing.T) {
	r, repository, service, rejected := manifestLayerRegistry(t)
	root, _ := manifestLayerFixture(t, r)
	snapshot := snapshotFor(t, r.store, []string{repository}, []v1.Descriptor{root})
	r.tags["bundle"] = snapshot.Descriptor
	path := filepath.Join(t.TempDir(), "layout")
	if _, err := service.Pull(ctx, repository+":bundle", path, PullLocked); err != nil {
		t.Fatal(err)
	}
	store, err := oci.NewStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range r.store.descriptors {
		expected, err := content.FetchAll(ctx, r.store, d)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := content.FetchAll(ctx, store, d)
		if err != nil {
			t.Fatalf("offline closure lost %s: %v", d.Digest, err)
		}
		if !bytes.Equal(actual, expected) {
			t.Fatalf("bytes changed for %s", d.Digest)
		}
	}
	if rejected.Load() != 0 {
		t.Fatal("pull requested a manifest through the blob endpoint")
	}
	if _, err := service.PlanPush(ctx, path, "destination.example/bundles/cache:v1", PushOptions{}); err != nil {
		t.Fatal(err)
	}
	// Every platform must remain required when planning from the offline layout.
	raw, err := content.FetchAll(ctx, r.store, root)
	if err != nil {
		t.Fatal(err)
	}
	var cache v1.Manifest
	if err := json.Unmarshal(raw, &cache); err != nil {
		t.Fatal(err)
	}
	raw, err = content.FetchAll(ctx, r.store, cache.Layers[0])
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	arm := index.Manifests[1]
	if err := os.Remove(filepath.Join(path, "blobs", arm.Digest.Algorithm().String(), arm.Digest.Encoded())); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PlanPush(ctx, path, "destination.example/bundles/cache:v1", PushOptions{}); err == nil {
		t.Fatal("planned layout missing the second platform beneath an index layer")
	}
}

func TestCopyAndRemoteVerificationTraverseManifestLayers(t *testing.T) {
	r, repository, service, rejected := manifestLayerRegistry(t)
	root, _ := manifestLayerFixture(t, r)
	remote, err := service.repository(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	dst := memory.New()
	if err := copyGraph(ctx, remote, dst, root, &Stats{}, nil); err != nil {
		t.Fatal(err)
	}
	for _, d := range r.store.descriptors {
		if _, err := content.FetchAll(ctx, dst, d); err != nil {
			t.Fatalf("copy lost descendant %s: %v", d.Digest, err)
		}
	}
	if rejected.Load() != 0 {
		t.Fatal("source copy requested manifest bytes through the blob endpoint")
	}
	if err := verifyRemoteClosure(ctx, remote, root); err == nil || !strings.Contains(err.Error(), "layer blob") {
		t.Fatalf("destination closure accepted missing layer blob registration: %v", err)
	}
	// Destination verification additionally requires layer blob registrations.
	// The ordinary reader exposes both endpoint roles for the same content.
	server := httptest.NewServer(http.HandlerFunc(r.serve))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	destinationService, err := NewService(Options{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	remote, err = destinationService.repository(context.Background(), host+"/test/artifacts")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyRemoteClosure(ctx, remote, root); err != nil {
		t.Fatal(err)
	}
	// A missing opaque descendant must also invalidate remote verification.
	var payload digest.Digest
	for key, d := range r.store.descriptors {
		if d.MediaType == "application/example" {
			payload = digest.Digest(key)
			break
		}
	}
	if payload == "" {
		t.Fatal("fixture has no image payload")
	}
	delete(r.store.descriptors, payload.String())
	if err := verifyRemoteClosure(ctx, remote, root); err == nil {
		t.Fatal("remote verification accepted a missing image payload below manifest layers")
	}
}

func TestManagedReferrerTagsIncludeManifestLayers(t *testing.T) {
	r, _, _, _ := manifestLayerRegistry(t)
	subject := object(t, r.store, "absent subject is weak")
	ref := refIndex(t, r, subject, []v1.Descriptor{})
	cfg := put(t, r.store, v1.MediaTypeEmptyJSON, []byte("{}"))
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{ref}})
	if err != nil {
		t.Fatal(err)
	}
	root := put(t, r.store, v1.MediaTypeImageManifest, raw)
	tags, err := managedReferrerTags(ctx, r.store, []DeliveryRepository{{Repository: "destination.example/cache", Roots: []v1.Descriptor{root}}})
	if err != nil {
		t.Fatal(err)
	}
	expected := strings.Replace(subject.Digest.String(), ":", "-", 1)
	if len(tags) != 1 || tags[0].reference != expected {
		t.Fatalf("layer referrer fallback tag not reserved: %+v", tags)
	}
}

func TestInlineManifestLayerMaterializesWithoutSourceManifest(t *testing.T) {
	r, repository, service, rejected := manifestLayerRegistry(t)
	root, envelopes := manifestLayerFixture(t, r)
	raw, err := content.FetchAll(ctx, r.store, root)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	index := envelopes[1]
	manifest.Layers[0].Data, err = content.FetchAll(ctx, r.store, index)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	root = put(t, r.store, v1.MediaTypeImageManifest, raw)
	r.tags["stable"] = root
	delete(r.store.descriptors, index.Digest.String())
	remote, err := service.repository(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	dst := memory.New()
	if err := copyGraph(ctx, remote, dst, root, &Stats{}, nil); err != nil {
		t.Fatalf("copy should use inline manifest data: %v", err)
	}
	if _, err := content.FetchAll(ctx, dst, index); err != nil {
		t.Fatalf("inline manifest not materialized: %v", err)
	}
	if err := bundle.VerifyClosure(ctx, dst, root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.capture(ctx, config.Recipe{Include: []config.Selection{{Ref: repository + ":stable", Referrers: policy("none")}}}); err != nil {
		t.Fatalf("capture should use inline manifest data: %v", err)
	}
	if rejected.Load() != 0 {
		t.Fatal("inline manifest requested through blob endpoint")
	}
	if err := verifyRemoteClosure(ctx, remote, root); err == nil {
		t.Fatal("remote destination verification accepted an unavailable inline child manifest")
	}
}

func TestPlanFindsNativeBundleInsideManifestLayer(t *testing.T) {
	store := memory.New()
	image := object(t, store, "nested bundle image")
	child := snapshotFor(t, store, []string{"source.example/images/api"}, []v1.Descriptor{image})
	cfg := put(t, store, v1.MediaTypeEmptyJSON, []byte("{}"))
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{child.Descriptor}})
	if err != nil {
		t.Fatal(err)
	}
	wrapper := put(t, store, v1.MediaTypeImageManifest, raw)
	outer := snapshotFor(t, store, []string{"source.example/artifacts/wrapper"}, []v1.Descriptor{wrapper})
	path := layoutFor(t, store, outer.Descriptor)
	plan, err := (&Service{}).PlanPush(ctx, path, "destination.example/bundles/release:v1", PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range plan.Tags {
		if tag.Repository == "destination.example/bundles/release/images/api" && tag.Reference == "stable" && tag.Descriptor.Digest == image.Digest {
			return
		}
	}
	t.Fatalf("native bundle layer lost child image delivery alias: %+v", plan.Tags)
}
