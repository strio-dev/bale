package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"oras.land/oras-go/v2/content"
)

type readRegistry struct {
	store     *testStore
	tags      map[string]v1.Descriptor
	refs      map[string][]v1.Descriptor
	headTags  map[string]int
	blobReads int
}

func (r *readRegistry) serve(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	if path == "/v2/" {
		w.WriteHeader(200)
		return
	}
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	for _, kind := range []string{"manifests", "blobs", "referrers"} {
		marker := "/" + kind + "/"
		if i := strings.Index(path, marker); i >= 0 {
			ref := path[i+len(marker):]
			if kind == "referrers" {
				w.Header().Set("Content-Type", v1.MediaTypeImageIndex)
				refs := r.refs[ref]
				if refs == nil {
					refs = []v1.Descriptor{}
				}
				_ = json.NewEncoder(w).Encode(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: refs})
				return
			}
			d, ok := r.tags[ref]
			if !ok {
				d, ok = r.store.descriptors[ref]
				if !ok {
					w.WriteHeader(404)
					return
				}
			}
			raw, err := content.FetchAll(req.Context(), r.store, d)
			if err != nil {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Type", d.MediaType)
			w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
			w.Header().Set("Docker-Content-Digest", d.Digest.String())
			if req.Method == http.MethodHead {
				if kind == "manifests" && ok {
					r.headTags[ref]++
				}
				w.WriteHeader(200)
				return
			}
			if kind == "blobs" {
				r.blobReads++
			}
			_, _ = w.Write(raw)
			return
		}
	}
	w.WriteHeader(404)
}
func testRegistry(t *testing.T) (*readRegistry, string, *Service) {
	t.Helper()
	r := &readRegistry{store: &testStore{metadataStore: newMetadataStore(), descriptors: map[string]v1.Descriptor{}}, tags: map[string]v1.Descriptor{}, refs: map[string][]v1.Descriptor{}, headTags: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	s, err := NewService(Options{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	return r, host + "/test/artifacts", s
}
func refIndex(t *testing.T, r *readRegistry, subject v1.Descriptor, children []v1.Descriptor) v1.Descriptor {
	t.Helper()
	raw, _ := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, ArtifactType: "application/example.dependencies", Subject: &subject, Manifests: children})
	d := put(t, r.store, v1.MediaTypeImageIndex, raw)
	r.refs[subject.Digest.String()] = append(r.refs[subject.Digest.String()], d)
	return d
}
func policy(include string, required ...v1.Descriptor) config.Policy {
	p := config.Policy{Include: include, Require: []config.RequiredReferrer{}}
	for _, d := range required {
		p.Require = append(p.Require, config.RequiredReferrer{Digest: d.Digest})
	}
	return p
}

func TestCapturePolicyUpgradeAndSingleResolution(t *testing.T) {
	r, repo, s := testRegistry(t)
	chart := object(t, r.store, "chart")
	image := object(t, r.store, "image")
	attachment := refIndex(t, r, chart, []v1.Descriptor{image})
	_ = refIndex(t, r, image, []v1.Descriptor{})
	r.tags["stable"] = chart
	recipe := config.Recipe{Include: []config.Selection{{Ref: repo + ":stable", Referrers: policy("none")}, {Ref: repo + ":stable", Referrers: policy("all")}}}
	g, snap, err := s.capture(ctx, recipe)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Metadata.Referrers) != 2 {
		t.Fatalf("policy upgrade lost associations: %+v", snap.Metadata.Referrers)
	}
	if r.headTags["stable"] != 1 {
		t.Fatalf("mutable tag resolved %d times", r.headTags["stable"])
	}
	if r.blobReads != 0 || g.stats.BlobBytesDownloaded != 0 {
		t.Fatal("capture read opaque payloads")
	}
	found := false
	for _, a := range snap.Metadata.Referrers {
		if a.Descriptor.Digest == attachment.Digest {
			found = true
		}
	}
	if !found {
		t.Fatal("missing prepared dependency attachment")
	}
}
func TestCaptureRequiredRootDoesNotPropagate(t *testing.T) {
	r, repo, s := testRegistry(t)
	chart := object(t, r.store, "chart")
	image := object(t, r.store, "image")
	attachment := refIndex(t, r, chart, []v1.Descriptor{image})
	_ = refIndex(t, r, image, []v1.Descriptor{})
	r.tags["stable"] = chart
	// Mandatory direct fetch works even when discovery listing omits the referrer.
	delete(r.refs, chart.Digest.String())
	recipe := config.Recipe{Include: []config.Selection{{Ref: repo + ":stable", Referrers: policy("none", attachment)}}}
	_, snap, err := s.capture(ctx, recipe)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Metadata.Referrers) != 1 {
		t.Fatalf("required policy propagated to descendants: %+v", snap.Metadata.Referrers)
	}
	wrong := refIndex(t, r, image, []v1.Descriptor{})
	recipe.Include[0].Referrers = policy("none", wrong)
	if _, _, err := s.capture(ctx, recipe); err == nil {
		t.Fatal("accepted wrong-subject mandatory referrer")
	}
}
func TestCaptureOpaquePayloadWithEnvelopeMediaType(t *testing.T) {
	r, repo, s := testRegistry(t)
	cfg := put(t, r.store, v1.MediaTypeImageManifest, []byte("opaque config, not a manifest"))
	raw, _ := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{}})
	root := put(t, r.store, v1.MediaTypeImageManifest, raw)
	r.tags["stable"] = root
	_, snap, err := s.capture(ctx, config.Recipe{Include: []config.Selection{{Ref: repo + ":stable", Referrers: policy("none")}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Metadata.Origins) != 1 {
		t.Fatal("opaque config treated as manifest origin")
	}
	dst := newMetadataStore()
	if err := copyGraph(ctx, r.store, dst, snap.Metadata.Roots[0].Descriptor, &Stats{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := bundle.VerifyClosure(ctx, dst, root); err != nil {
		t.Fatal(err)
	}
}

type testStore struct {
	*metadataStore
	descriptors map[string]v1.Descriptor
}

func (s *testStore) Push(ctx context.Context, d v1.Descriptor, r io.Reader) error {
	s.descriptors[d.Digest.String()] = d
	return s.metadataStore.Push(ctx, d, r)
}

func TestCreateUnknownRootEnvelopeReturnsTypedUnsupported(t *testing.T) {
	source, name, service := testRegistry(t)
	unknown := put(t, source.store, "application/example.unknown-envelope", []byte(`{"content":"unknown"}`))
	source.tags["unknown"] = unknown
	_, err := service.Create(ctx, config.Recipe{Include: []config.Selection{{Ref: name + ":unknown", Referrers: policy("none")}}}, name+":capture", CreateOptions{DryRun: true})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported root lost error type: %v", err)
	}
}
