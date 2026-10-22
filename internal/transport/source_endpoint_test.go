package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
)

func TestEnvelopeSourceBlobFallback(t *testing.T) {
	raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	d := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	for _, status := range []int{404, 403, 401, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			blobs := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/manifests/") {
					if status != 200 {
						http.Error(w, "manifest unavailable", status)
						return
					}
					w.Header().Set("Content-Type", d.MediaType)
					w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
					_, _ = w.Write(bytes.Repeat([]byte("!"), len(raw)))
					return
				}
				blobs++
				w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			repo, _ := remote.NewRepository(strings.TrimPrefix(server.URL, "http://") + "/test")
			repo.PlainHTTP = true
			got, err := fetchEnvelopeBytes(ctx, repo, d)
			if status == 404 {
				if err != nil || !bytes.Equal(got, raw) || blobs != 1 {
					t.Fatalf("fallback: bytes=%s err=%v blob requests=%d", got, err, blobs)
				}
			} else if err == nil || blobs != 0 {
				t.Fatalf("masked status %d/integrity error: %v blob requests=%d", status, err, blobs)
			}
		})
	}
}

func TestRemoteClosureRequiresLayerBlobAndRepairsReuse(t *testing.T) {
	src := memory.New()
	child := object(t, src, "child payload")
	cfg := put(t, src, v1.MediaTypeEmptyJSON, []byte("{}"))
	parentRaw, _ := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{child}})
	parent := put(t, src, v1.MediaTypeImageManifest, parentRaw)
	// Visit child as an index member before finding it again in a layer position.
	rootRaw, _ := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{child, parent}})
	root := put(t, src, v1.MediaTypeImageIndex, rootRaw)
	manifests, blobs := map[string][]byte{}, map[string][]byte{}
	descriptors := []v1.Descriptor{root, parent, child, cfg}
	var cm v1.Manifest
	raw, _ := content.FetchAll(ctx, src, child)
	_ = json.Unmarshal(raw, &cm)
	descriptors = append(descriptors, cm.Layers...)
	for _, d := range descriptors {
		raw, err := content.FetchAll(ctx, src, d)
		if err != nil {
			t.Fatal(err)
		}
		if d.MediaType == v1.MediaTypeImageManifest || d.MediaType == v1.MediaTypeImageIndex {
			manifests[d.Digest.String()] = raw
		} else {
			blobs[d.Digest.String()] = raw
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v2/test/")
		if path == "blobs/uploads/" {
			w.Header().Set("Location", "/v2/test/blobs/uploads/id")
			w.WriteHeader(202)
			return
		}
		if path == "blobs/uploads/id" {
			raw, _ := io.ReadAll(r.Body)
			key := r.URL.Query().Get("digest")
			blobs[key] = raw
			w.Header().Set("Docker-Content-Digest", key)
			w.Header().Set("Location", "/v2/test/blobs/"+key)
			w.WriteHeader(201)
			return
		}
		kind, key, ok := strings.Cut(path, "/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		objects := blobs
		if kind == "manifests" {
			objects = manifests
		}
		raw, ok := objects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if kind == "manifests" {
			var m struct {
				MediaType string `json:"mediaType"`
			}
			_ = json.Unmarshal(raw, &m)
			w.Header().Set("Content-Type", m.MediaType)
		}
		w.Header().Set("Docker-Content-Digest", key)
		w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(raw)
		}
	}))
	defer server.Close()
	repo, _ := remote.NewRepository(strings.TrimPrefix(server.URL, "http://") + "/test")
	repo.PlainHTTP = true
	if err := verifyRemoteClosure(ctx, repo, root); err == nil || !strings.Contains(err.Error(), "layer blob") {
		t.Fatalf("missing dual registration accepted: %v", err)
	}
	var stats Stats
	if err := copyGraph(ctx, src, repo, root, &stats, nil); err != nil {
		t.Fatal(err)
	}
	if err := verifyRemoteClosure(ctx, repo, root); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(blobs[child.Digest.String()], manifests[child.Digest.String()]) {
		t.Fatal("repair changed bytes")
	}
}

func TestCopyBlobOnlyEnvelopeLayer(t *testing.T) {
	source := memory.New()
	child := object(t, source, "blob-only child")
	cfg := put(t, source, v1.MediaTypeEmptyJSON, []byte("{}"))
	raw, _ := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{child}})
	parent := put(t, source, v1.MediaTypeImageManifest, raw)
	var inner v1.Manifest
	childRaw, _ := content.FetchAll(ctx, source, child)
	_ = json.Unmarshal(childRaw, &inner)
	objects := map[string][]byte{}
	types := map[string]string{}
	for _, d := range append([]v1.Descriptor{parent, child, cfg}, inner.Layers...) {
		objects[d.Digest.String()], _ = content.FetchAll(ctx, source, d)
		types[d.Digest.String()] = d.MediaType
	}
	blobChildReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind, key, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/test/"), "/")
		if !ok || kind == "manifests" && key == child.Digest.String() {
			http.NotFound(w, r)
			return
		}
		raw, ok := objects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if kind == "blobs" && key == child.Digest.String() {
			blobChildReads++
		}
		if kind == "manifests" {
			w.Header().Set("Content-Type", types[key])
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
		w.Header().Set("Docker-Content-Digest", key)
		if r.Method != http.MethodHead {
			_, _ = w.Write(raw)
		}
	}))
	defer server.Close()
	repo, _ := remote.NewRepository(strings.TrimPrefix(server.URL, "http://") + "/test")
	repo.PlainHTTP = true
	destination := memory.New()
	if err := copyGraph(ctx, repo, destination, parent, &Stats{}, nil); err != nil {
		t.Fatal(err)
	}
	if blobChildReads != 1 {
		t.Fatalf("expected child blob fallback, got %d reads", blobChildReads)
	}
	if err := bundle.VerifyClosure(ctx, destination, parent); err != nil {
		t.Fatal(err)
	}
}
