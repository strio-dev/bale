package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/config"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

// The parent deliberately lies about its payload size. HEAD must validate the
// descriptor rather than merely prove that the digest exists in the repository.
func wrongSizeRoot(t *testing.T, r *readRegistry) v1.Descriptor {
	t.Helper()
	cfg := put(t, r.store, "application/example.opaque", []byte("payload"))
	cfg.Size++
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{}})
	if err != nil {
		t.Fatal(err)
	}
	return put(t, r.store, v1.MediaTypeImageManifest, raw)
}

func TestCreateRejectsWrongSourcePayloadSizeBeforePublication(t *testing.T) {
	r := &readRegistry{store: &testStore{metadataStore: newMetadataStore(), descriptors: map[string]v1.Descriptor{}}, tags: map[string]v1.Descriptor{}, refs: map[string][]v1.Descriptor{}, headTags: map[string]int{}}
	var writes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			writes.Add(1)
		}
		r.serve(w, req)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	repo := host + "/test/artifacts"
	s, err := NewService(Options{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	r.tags["stable"] = wrongSizeRoot(t, r)
	_, err = s.Create(ctx, config.Recipe{Include: []config.Selection{{Ref: repo + ":stable", Referrers: policy("none")}}}, repo+":complete", CreateOptions{})
	if err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("wanted source descriptor size rejection, got %v", err)
	}
	if _, published := r.tags["complete"]; published {
		t.Fatal("published completion for invalid source graph")
	}
	if writes.Load() != 0 {
		t.Fatal("invalid source graph caused a destination write")
	}
	if r.blobReads != 0 {
		t.Fatal("size verification downloaded a payload instead of using HEAD")
	}
}

func TestCopyRejectsWrongExistingDestinationPayloadSize(t *testing.T) {
	r, repo, s := testRegistry(t)
	root := wrongSizeRoot(t, r)
	dst, err := s.repository(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyGraph(ctx, r.store, dst, root, &Stats{}, nil); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("wanted destination descriptor size rejection, got %v", err)
	}
	if r.blobReads != 0 {
		t.Fatal("destination validation downloaded payload")
	}
}

func TestVerifyRemoteClosureRejectsWrongPayloadSize(t *testing.T) {
	r, repo, s := testRegistry(t)
	root := wrongSizeRoot(t, r)
	dst, err := s.repository(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyRemoteClosure(ctx, dst, root); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("wanted closure descriptor size rejection, got %v", err)
	}
	if r.blobReads != 0 {
		t.Fatal("closure validation downloaded payload")
	}
}

func TestCopyValidatesSuccessfulMountDescriptor(t *testing.T) {
	r := &readRegistry{store: &testStore{metadataStore: newMetadataStore(), descriptors: map[string]v1.Descriptor{}}, tags: map[string]v1.Descriptor{}, refs: map[string][]v1.Descriptor{}, headTags: map[string]int{}}
	root := wrongSizeRoot(t, r)
	var mounted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.URL.Query().Get("mount") != "" {
			mounted.Store(true)
			w.WriteHeader(http.StatusCreated)
			return
		}
		if req.Method == http.MethodHead && strings.Contains(req.URL.Path, "/blobs/") && !mounted.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		r.serve(w, req)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	s, err := NewService(Options{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	dst, err := s.repository(host + "/target")
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	raw, err := content.FetchAll(ctx, r.store, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	err = copyGraph(ctx, r.store, dst, root, &Stats{}, map[digest.Digest]string{manifest.Config.Digest: host + "/source"})
	if err == nil || !strings.Contains(err.Error(), "size") || !mounted.Load() {
		t.Fatalf("successful mount bypassed descriptor validation: %v (mounted %t)", err, mounted.Load())
	}
	if r.blobReads != 0 {
		t.Fatal("mount validation downloaded payload")
	}
}

type closeErrorFetcher struct {
	content.Fetcher
	target digest.Digest
	err    error
}

func (f closeErrorFetcher) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	rc, err := f.Fetcher.Fetch(ctx, d)
	if err != nil || d.Digest != f.target {
		return rc, err
	}
	return closeErrorReader{ReadCloser: rc, err: f.err}, nil
}

type closeErrorReader struct {
	io.ReadCloser
	err error
}

func (r closeErrorReader) Close() error { return errors.Join(r.ReadCloser.Close(), r.err) }

type rejectingStorage struct {
	content.Storage
	err error
}

func (s rejectingStorage) Push(context.Context, v1.Descriptor, io.Reader) error { return s.err }

func TestCopyReportsPayloadCloseErrorAndPreservesPrimaryError(t *testing.T) {
	src := memory.New()
	root := object(t, src, "payload")
	raw, err := content.FetchAll(ctx, src, root)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	closeErr := errors.New("payload close failure")
	fetcher := closeErrorFetcher{Fetcher: src, target: manifest.Config.Digest, err: closeErr}
	if err := copyGraph(ctx, fetcher, memory.New(), root, &Stats{}, nil); !errors.Is(err, closeErr) {
		t.Fatalf("lost close error: %v", err)
	}
	pushErr := errors.New("payload push failure")
	err = copyGraph(ctx, fetcher, rejectingStorage{Storage: memory.New(), err: pushErr}, root, &Stats{}, nil)
	if !errors.Is(err, closeErr) || !errors.Is(err, pushErr) {
		t.Fatalf("did not preserve primary and close errors: %v", err)
	}
}

func TestCreateDryRunEmitsAccountingSemantics(t *testing.T) {
	r, repo, s := testRegistry(t)
	r.tags["stable"] = object(t, r.store, "payload")
	result, err := s.Create(ctx, config.Recipe{Include: []config.Selection{{Ref: repo + ":stable", Referrers: policy("none")}}}, repo+":complete", CreateOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stats.Accounting != statsAccounting || !strings.Contains(string(raw), `"accounting":"Graph content accounting, not total network traffic:`) {
		t.Fatalf("missing visible accounting description: %s", raw)
	}
}

func TestExplicitRepositoryMappingStaysUnderBundleDestination(t *testing.T) {
	src := memory.New()
	a := object(t, src, "first")
	b := object(t, src, "second")
	snap := snapshotFor(t, src, []string{"one.example/team/api", "two.example/team/api"}, []v1.Descriptor{a, b})
	path := layoutFor(t, src, snap.Descriptor)
	plan, err := (&Service{}).PlanPush(ctx, path, "dest.example/bundles/app:v1", PushOptions{RepositoryMap: map[string]string{"two.example/team/api": "vendor/a"}})
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "dest.example/bundles/app/"
	if got := plan.RepositoryMap["two.example/team/api"]; got != prefix+"vendor/a" {
		t.Fatalf("relative mapping escaped selected bundle repository: %s", got)
	}
	for _, repository := range plan.RepositoryMap {
		if !strings.HasPrefix(repository, prefix) {
			t.Fatalf("planned mapping outside bundle destination: %s", repository)
		}
	}
	for _, repository := range plan.Repositories {
		if !strings.HasPrefix(repository.Repository, prefix) {
			t.Fatalf("planned repository outside bundle destination: %s", repository.Repository)
		}
	}
	for _, tag := range plan.Tags {
		if !strings.HasPrefix(tag.Repository, prefix) {
			t.Fatalf("planned tag outside bundle destination: %s", tag.Repository)
		}
	}
}

func TestCaptureSharesInlinePayloadAcrossSourceRepositories(t *testing.T) {
	for _, inline := range []string{"config", "layer"} {
		t.Run(inline, func(t *testing.T) {
			r, repo, s := testRegistry(t)
			configBytes, layerBytes := []byte("opaque configuration"), []byte("opaque layer")
			cfg := put(t, r.store, "application/example.config", configBytes)
			layer := put(t, r.store, "application/example.layer", layerBytes)
			if inline == "config" {
				cfg.Data = configBytes
			} else {
				layer.Data = layerBytes
			}
			raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{layer}})
			if err != nil {
				t.Fatal(err)
			}
			r.tags["stable"] = put(t, r.store, v1.MediaTypeImageManifest, raw)
			other := strings.TrimSuffix(repo, "/test/artifacts") + "/other/artifacts"
			recipe := config.Recipe{Include: []config.Selection{{Ref: repo + ":stable", Referrers: policy("none")}, {Ref: other + ":stable", Referrers: policy("none")}}}
			_, snapshot, err := s.capture(ctx, recipe)
			if err != nil {
				t.Fatalf("shared inline %s rejected: %v", inline, err)
			}
			if len(snapshot.Metadata.Roots) != 2 || len(snapshot.Metadata.Origins) != 2 {
				t.Fatalf("source coordinates lost: %+v", snapshot.Metadata)
			}
			if r.blobReads != 0 {
				t.Fatal("capture downloaded an inline or reusable payload")
			}
		})
	}
}

func TestCaptureRecordsOriginOnceForRootAndReferrerViews(t *testing.T) {
	r, repo, s := testRegistry(t)
	chart := object(t, r.store, "chart")
	attachment := refIndex(t, r, chart, []v1.Descriptor{})
	r.tags["attachment"] = attachment
	r.tags["chart"] = chart
	// Registry tag resolution omits artifactType, while referrers discovery
	// supplies the artifact type from the same immutable index body.
	discovered := attachment
	discovered.ArtifactType = "application/example.dependencies"
	r.refs[chart.Digest.String()] = []v1.Descriptor{discovered}
	recipe := config.Recipe{Include: []config.Selection{{Ref: repo + ":attachment", Referrers: policy("none")}, {Ref: repo + ":chart", Referrers: policy("all")}}}
	_, snapshot, err := s.capture(ctx, recipe)
	if err != nil {
		t.Fatalf("valid root/referrer descriptor views rejected: %v", err)
	}
	if len(snapshot.Metadata.Origins) != 2 || len(snapshot.Metadata.Referrers) != 1 {
		t.Fatalf("duplicated origin or lost referrer: %+v", snapshot.Metadata)
	}
	// Deduplicating an origin must still validate each descriptor view.
	discovered.ArtifactType = "application/example.wrong"
	r.refs[chart.Digest.String()] = []v1.Descriptor{discovered}
	if _, _, err := s.capture(ctx, recipe); err == nil || !strings.Contains(err.Error(), "artifactType") {
		t.Fatalf("alternate descriptor view escaped validation: %v", err)
	}
}

func TestRepositoryReferrerFallbackUpdateNeverDeletesOldIndex(t *testing.T) {
	store := memory.New()
	subject := object(t, store, "subject")
	cfg := put(t, store, v1.MediaTypeEmptyJSON, []byte("{}"))
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{}, Subject: &subject})
	if err != nil {
		t.Fatal(err)
	}
	referrer := put(t, store, v1.MediaTypeImageManifest, raw)
	oldIndex, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{}})
	if err != nil {
		t.Fatal(err)
	}
	fallbackPath := "/v2/test/manifests/" + strings.Replace(subject.Digest.String(), ":", "-", 1)
	oldIndexPath := "/v2/test/manifests/" + digest.FromBytes(oldIndex).String()
	var deletes atomic.Int64
	var updated atomic.Bool
	var inventoryReads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusAccepted)
		case (req.Method == http.MethodGet || req.Method == http.MethodHead) && (req.URL.Path == fallbackPath || req.URL.Path == oldIndexPath):
			w.Header().Set("Content-Type", v1.MediaTypeImageIndex)
			w.Header().Set("Docker-Content-Digest", digest.FromBytes(oldIndex).String())
			w.Header().Set("Content-Length", fmt.Sprint(len(oldIndex)))
			if req.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			inventoryReads.Add(1)
			_, _ = w.Write(oldIndex)
		case req.Method == http.MethodPut:
			body, err := io.ReadAll(req.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req.URL.Path == fallbackPath {
				updated.Store(true)
			}
			w.Header().Set("Docker-Content-Digest", digest.FromBytes(body).String())
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	s, err := NewService(Options{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.repository(host + "/test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.preflightManagedTags(ctx, store, []DeliveryRepository{{Repository: host + "/test", Roots: []v1.Descriptor{referrer}}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(ctx, referrer, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if !updated.Load() {
		t.Fatal("referrer push did not exercise fallback inventory update")
	}
	if deletes.Load() != 0 {
		t.Fatal("referrer publication deleted the previous inventory by digest")
	}
	if inventoryReads.Load() < 2 {
		t.Fatal("existing fallback inventory was not read during preflight and update")
	}
}
