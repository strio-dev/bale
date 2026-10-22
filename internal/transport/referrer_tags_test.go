package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func monitoredRegistry(t *testing.T) (*readRegistry, string, *Service, *atomic.Int64) {
	t.Helper()
	r := &readRegistry{store: &testStore{metadataStore: newMetadataStore(), descriptors: map[string]v1.Descriptor{}}, tags: map[string]v1.Descriptor{}, refs: map[string][]v1.Descriptor{}, headTags: map[string]int{}}
	writes := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			writes.Add(1)
		}
		r.serve(w, req)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	s, err := NewService(Options{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	return r, host + "/test", s, writes
}

func subjectObject(t *testing.T, store content.Storage) (v1.Descriptor, v1.Descriptor) {
	t.Helper()
	subject := object(t, store, "subject")
	cfg := put(t, store, v1.MediaTypeEmptyJSON, []byte("{}"))
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{}, Subject: &subject})
	if err != nil {
		t.Fatal(err)
	}
	return subject, put(t, store, v1.MediaTypeImageManifest, raw)
}

func TestCreateRejectsStructuralSubjectCompletionCollisionBeforeWrites(t *testing.T) {
	r, repo, s, writes := monitoredRegistry(t)
	subject, attachment := subjectObject(t, r.store)
	raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{attachment}})
	if err != nil {
		t.Fatal(err)
	}
	r.tags["stable"] = put(t, r.store, v1.MediaTypeImageIndex, raw)
	managed := strings.Replace(subject.Digest.String(), ":", "-", 1)
	_, err = s.Create(ctx, config.Recipe{Include: []config.Selection{{Ref: repo + ":stable", Referrers: policy("none")}}}, repo+":"+managed, CreateOptions{})
	if err == nil || !strings.Contains(err.Error(), "managed referrer tag") {
		t.Fatalf("structural-only subject completion collision not rejected: %v", err)
	}
	if writes.Load() != 0 {
		t.Fatal("collision caused registry mutation")
	}
}

func TestPlanRejectsManagedCompletionAndAllTagAliasCollisions(t *testing.T) {
	for _, collision := range []string{"completion", "all-tags alias"} {
		t.Run(collision, func(t *testing.T) {
			store := memory.New()
			subject, attachment := subjectObject(t, store)
			managed := strings.Replace(subject.Digest.String(), ":", "-", 1)
			snapshot := snapshotFor(t, store, []string{"source.example/team/app"}, []v1.Descriptor{attachment})
			dest := "dest.example/bundles/app:" + managed
			if collision == "all-tags alias" {
				m := snapshot.Metadata
				tags := []string{}
				m.Recipe.Include[0] = config.Selection{Repository: "source.example/team/app", Tags: &tags, Referrers: policy("none"), Dependencies: m.Recipe.Include[0].Dependencies}
				m.Roots[0].Reference = managed
				var err error
				snapshot, err = bundle.BuildSnapshot(ctx, store, m)
				if err != nil {
					t.Fatal(err)
				}
				dest = "dest.example/bundles/app:complete"
			}
			path := layoutFor(t, store, snapshot.Descriptor)
			if _, err := (&Service{}).PlanPush(ctx, path, dest, PushOptions{}); err == nil || !strings.Contains(err.Error(), "managed referrer tag") {
				t.Fatalf("%s collision not rejected: %v", collision, err)
			}
		})
	}
}

func TestCreateValidatesPreexistingManagedInventoryBeforeWrites(t *testing.T) {
	for _, kind := range []string{"arbitrary object", "wrong subject", "missing child", "wrong child size", "valid inventory"} {
		t.Run(kind, func(t *testing.T) {
			r, repo, s, writes := monitoredRegistry(t)
			subject, attachment := subjectObject(t, r.store)
			r.tags["stable"] = attachment
			managed := strings.Replace(subject.Digest.String(), ":", "-", 1)
			inventory := attachment
			if kind != "arbitrary object" {
				child := attachment
				if kind == "wrong subject" {
					child = object(t, r.store, "not a referrer")
				}
				if kind == "missing child" {
					child = object(t, r.store, "missing inventory child")
					delete(r.store.descriptors, child.Digest.String())
				}
				if kind == "wrong child size" {
					child.Size++
				}
				raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{child}})
				if err != nil {
					t.Fatal(err)
				}
				inventory = put(t, r.store, v1.MediaTypeImageIndex, raw)
			}
			r.tags[managed] = inventory
			_, err := s.Create(ctx, config.Recipe{Include: []config.Selection{{Ref: repo + ":stable", Referrers: policy("none")}}}, repo+":complete", CreateOptions{DryRun: true})
			if kind == "valid inventory" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "managed referrer tag") {
				t.Fatalf("invalid existing inventory accepted: %v", err)
			}
			if writes.Load() != 0 {
				t.Fatal("inventory preflight caused registry mutation")
			}
		})
	}
}

func TestPushPreflightsManagedInventoryBeforeAnyWrites(t *testing.T) {
	r, repo, s, writes := monitoredRegistry(t)
	subject, attachment := subjectObject(t, r.store)
	snapshot := snapshotFor(t, r.store, []string{"source.example/team/app"}, []v1.Descriptor{attachment})
	path := layoutFor(t, r.store, snapshot.Descriptor)
	managed := strings.Replace(subject.Digest.String(), ":", "-", 1)
	r.tags[managed] = object(t, r.store, "arbitrary preexisting tag")
	plan, err := s.PlanPush(ctx, path, repo+":complete", PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Push(ctx, plan); err == nil || !strings.Contains(err.Error(), "managed referrer tag") {
		t.Fatalf("push accepted incompatible managed tag: %v", err)
	}
	if writes.Load() != 0 {
		t.Fatal("push mutated registry before inventory preflight finished")
	}
}

func TestPullPreservesExistingDanglingSymlink(t *testing.T) {
	r, repo, s := testRegistry(t)
	root := object(t, r.store, "payload")
	snapshot := snapshotFor(t, r.store, []string{repo}, []v1.Descriptor{root})
	r.tags["bundle"] = snapshot.Descriptor
	path := filepath.Join(t.TempDir(), "layout")
	linkTarget := filepath.Join(t.TempDir(), "absent")
	if err := os.Symlink(linkTarget, path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pull(ctx, repo+":bundle", path, PullLocked); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing symlink was not rejected: %v", err)
	}
	if got, err := os.Readlink(path); err != nil || got != linkTarget {
		t.Fatalf("existing symlink changed: %q, %v", got, err)
	}
}

func TestCreateRejectsManagedTagPointingToSubjectItself(t *testing.T) {
	r, repo, s, writes := monitoredRegistry(t)
	raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{}})
	if err != nil {
		t.Fatal(err)
	}
	subject := put(t, r.store, v1.MediaTypeImageIndex, raw)
	cfg := put(t, r.store, v1.MediaTypeEmptyJSON, []byte("{}"))
	raw, err = json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{}, Subject: &subject})
	if err != nil {
		t.Fatal(err)
	}
	r.tags["stable"] = put(t, r.store, v1.MediaTypeImageManifest, raw)
	managed := strings.Replace(subject.Digest.String(), ":", "-", 1)
	r.tags[managed] = subject
	_, err = s.Create(ctx, config.Recipe{Include: []config.Selection{{Ref: repo + ":stable", Referrers: policy("none")}}}, repo+":complete", CreateOptions{})
	if err == nil || !strings.Contains(err.Error(), "managed referrer tag") {
		t.Fatalf("managed tag pointing to empty index subject was accepted: %v", err)
	}
	if writes.Load() != 0 {
		t.Fatal("subject alias collision caused registry mutation")
	}
	if got := r.tags[managed]; got.Digest != subject.Digest {
		t.Fatal("subject alias changed")
	}
}
