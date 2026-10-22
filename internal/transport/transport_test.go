package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

var ctx = context.Background()

func put(t *testing.T, store content.Storage, mt string, raw []byte) v1.Descriptor {
	t.Helper()
	d := v1.Descriptor{MediaType: mt, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	if err := store.Push(ctx, d, bytes.NewReader(raw)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		t.Fatal(err)
	}
	return d
}
func object(t *testing.T, store content.Storage, payload string) v1.Descriptor {
	t.Helper()
	cfg := put(t, store, v1.MediaTypeEmptyJSON, []byte("{}"))
	layer := put(t, store, "application/example", []byte(payload))
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	return put(t, store, v1.MediaTypeImageManifest, raw)
}
func snapshotFor(t *testing.T, store content.Storage, repos []string, roots []v1.Descriptor) bundle.Snapshot {
	t.Helper()
	m := bundle.Metadata{SchemaVersion: 1, Recipe: config.Recipe{Include: []config.Selection{}}, Roots: []bundle.Root{}, Origins: []bundle.Origin{}, Referrers: []bundle.Association{}}
	for i, r := range roots {
		repo := repos[i]
		p := dependency.Policy{Follow: "none"}
		m.Recipe.Include = append(m.Recipe.Include, config.Selection{Ref: repo + ":stable", Referrers: config.Policy{Include: "none", Require: []config.RequiredReferrer{}}, Dependencies: p})
		m.Roots = append(m.Roots, bundle.Root{Input: i, Repository: repo, Reference: "stable", Descriptor: r})
		m.Origins = append(m.Origins, bundle.Origin{Repository: repo, Descriptor: r})
		m.Dependencies = append(m.Dependencies, bundle.DependencyResolution{Repository: repo, Subject: r, Policy: p, Outcome: "disabled", Dependencies: []bundle.ResolvedDependency{}})
	}
	s, err := bundle.BuildSnapshot(ctx, store, m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func layoutFor(t *testing.T, src content.Storage, root v1.Descriptor) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "layout")
	store, err := oci.NewStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyGraph(ctx, src, store, root, &Stats{}, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{root}})
	if err := os.WriteFile(filepath.Join(path, "index.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCopyTraversesExistingParentAndRetry(t *testing.T) {
	src := memory.New()
	root := object(t, src, "payload")
	dst := memory.New()
	raw, err := content.FetchAll(ctx, src, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.Push(ctx, root, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	failing := &failStorage{Storage: dst, remaining: 1}
	if err := copyGraph(ctx, src, failing, root, &Stats{}, nil); err == nil {
		t.Fatal("expected interruption")
	}
	if err := copyGraph(ctx, src, dst, root, &Stats{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := bundle.VerifyClosure(ctx, dst, root); err != nil {
		t.Fatal(err)
	}
	stats := Stats{}
	if err := copyGraph(ctx, src, dst, root, &stats, nil); err != nil {
		t.Fatal(err)
	}
	if stats.BlobBytesUploaded != 0 || stats.ManifestBytesUploaded != 0 {
		t.Fatalf("retry uploaded bytes: %+v", stats)
	}
}

type failStorage struct {
	content.Storage
	remaining int
}

func (f *failStorage) Push(ctx context.Context, d v1.Descriptor, r io.Reader) error {
	f.remaining--
	if f.remaining == 0 {
		return errors.New("interrupted transfer")
	}
	return f.Storage.Push(ctx, d, r)
}

type corruptFetcher struct {
	content.Fetcher
	target digest.Digest
}

func (f corruptFetcher) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	if d.Digest == f.target {
		return io.NopCloser(strings.NewReader(strings.Repeat("!", int(d.Size)))), nil
	}
	return f.Fetcher.Fetch(ctx, d)
}
func TestCopyRejectsCorruptPayload(t *testing.T) {
	src := memory.New()
	root := object(t, src, "payload")
	raw, _ := content.FetchAll(ctx, src, root)
	env, _ := bundle.ParseEnvelope(root, raw)
	bad := corruptFetcher{Fetcher: src, target: env.Children[1].Digest}
	if err := copyGraph(ctx, bad, memory.New(), root, &Stats{}, nil); err == nil {
		t.Fatal("accepted altered payload")
	}
}
func TestPlanCollisionAndExplicitMapping(t *testing.T) {
	src := memory.New()
	a := object(t, src, "a")
	b := object(t, src, "b")
	snap := snapshotFor(t, src, []string{"one.example/team/api", "two.example/team/api"}, []v1.Descriptor{a, b})
	path := layoutFor(t, src, snap.Descriptor)
	service := &Service{}
	if _, err := service.PlanPush(ctx, path, "dest.example/bundles/app:v1", PushOptions{}); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("wanted repository collision, got %v", err)
	}
	plan, err := service.PlanPush(ctx, path, "dest.example/bundles/app:v1", PushOptions{RepositoryMap: map[string]string{"two.example/team/api": "second/api"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Repositories) != 2 || len(plan.Tags) != 2 {
		t.Fatalf("unexpected plan %+v", plan)
	}
	plan.Destination = "dest.example/changed:v2"
	if _, err := service.Push(ctx, plan); err == nil || !strings.Contains(err.Error(), "plan changed") {
		t.Fatalf("mutated plan accepted: %v", err)
	}
}
func TestPlanNestedBundleRestoresChildAliases(t *testing.T) {
	src := memory.New()
	image := object(t, src, "image")
	child := snapshotFor(t, src, []string{"source.example/images/api"}, []v1.Descriptor{image})
	outer := snapshotFor(t, src, []string{"source.example/bundles/child"}, []v1.Descriptor{child.Descriptor})
	path := layoutFor(t, src, outer.Descriptor)
	plan, err := (&Service{}).PlanPush(ctx, path, "dest.example/bundles/release:v1", PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tags) != 2 {
		t.Fatalf("missing nested aliases: %+v", plan.Tags)
	}
	found := false
	for _, tag := range plan.Tags {
		if tag.Repository == "dest.example/bundles/release/images/api" && tag.Reference == "stable" && tag.Descriptor.Digest == image.Digest {
			found = true
		}
	}
	if !found {
		t.Fatal("nested image alias not mapped")
	}
}
func TestPlanRejectsMissingPayload(t *testing.T) {
	src := memory.New()
	root := object(t, src, "payload")
	snap := snapshotFor(t, src, []string{"source.example/api"}, []v1.Descriptor{root})
	path := layoutFor(t, src, snap.Descriptor)
	raw, _ := content.FetchAll(ctx, src, root)
	env, _ := bundle.ParseEnvelope(root, raw)
	d := env.Children[1]
	if err := os.Remove(filepath.Join(path, "blobs", d.Digest.Algorithm().String(), d.Digest.Encoded())); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Service{}).PlanPush(ctx, path, "dest.example/app:v1", PushOptions{}); err == nil {
		t.Fatal("planned incomplete layout")
	}
}
