package transport

import (
	"bytes"
	"context"
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
	"oras.land/oras-go/v2/registry/remote"
)

// Separate manifest/blob maps model registries which validate layers through
// their blob service even when the referenced bytes are already a manifest.
func TestCopyRegistersEnvelopeLayersAsBlobs(t *testing.T) {
	for _, mode := range []struct{ reuse, inline bool }{{}, {reuse: true}, {inline: true}} {
		t.Run(fmt.Sprintf("reuse=%v/inline=%v", mode.reuse, mode.inline), func(t *testing.T) {
			source, sourceName, service, rejected := manifestLayerRegistry(t)
			root, _ := manifestLayerFixture(t, source)
			indexParent := root
			if mode.inline {
				var err error
				root.Data, err = content.FetchAll(ctx, source.store, root)
				if err != nil {
					t.Fatal(err)
				}
			}
			cfg := put(t, source.store, v1.MediaTypeEmptyJSON, []byte("{}"))
			outer, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{root}})
			if err != nil {
				t.Fatal(err)
			}
			root = put(t, source.store, v1.MediaTypeImageManifest, outer)
			src, err := service.repository(context.Background(), sourceName)
			if err != nil {
				t.Fatal(err)
			}
			manifests, blobs := map[string][]byte{}, map[string][]byte{}
			if mode.reuse {
				for key, desc := range source.store.descriptors {
					if bundle.IsEnvelope(desc.MediaType) {
						manifests[key], err = content.FetchAll(ctx, source.store, desc)
						if err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if mode.inline {
				delete(source.store.descriptors, indexParent.Digest.String())
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				path := strings.TrimPrefix(req.URL.Path, "/v2/test/")
				if path == "blobs/uploads/" && req.Method == http.MethodPost {
					w.Header().Set("Location", "/v2/test/blobs/uploads/one")
					w.WriteHeader(http.StatusAccepted)
					return
				}
				if path == "blobs/uploads/one" && req.Method == http.MethodPut {
					raw, _ := io.ReadAll(req.Body)
					key := req.URL.Query().Get("digest")
					if digest.FromBytes(raw).String() != key {
						http.Error(w, "digest mismatch", 400)
						return
					}
					blobs[key] = raw
					w.Header().Set("Docker-Content-Digest", key)
					w.Header().Set("Location", "/v2/test/blobs/"+key)
					w.WriteHeader(http.StatusCreated)
					return
				}
				parts := strings.SplitN(path, "/", 2)
				if len(parts) != 2 {
					http.NotFound(w, req)
					return
				}
				kind, key := parts[0], parts[1]
				objects := blobs
				if kind == "manifests" {
					objects = manifests
				}
				if kind == "manifests" && req.Method == http.MethodPut {
					raw, _ := io.ReadAll(req.Body)
					if req.Header.Get("Content-Type") == v1.MediaTypeImageManifest {
						var m v1.Manifest
						if err := json.Unmarshal(raw, &m); err != nil {
							http.Error(w, err.Error(), 400)
							return
						}
						for _, layer := range append([]v1.Descriptor{m.Config}, m.Layers...) {
							if _, ok := blobs[layer.Digest.String()]; !ok {
								http.Error(w, "MANIFEST_BLOB_UNKNOWN", 400)
								return
							}
						}
					}
					manifests[key] = raw
					w.Header().Set("Docker-Content-Digest", key)
					w.Header().Set("Location", "/v2/test/manifests/"+key)
					w.WriteHeader(http.StatusCreated)
					return
				}
				raw, ok := objects[key]
				if !ok {
					http.NotFound(w, req)
					return
				}
				w.Header().Set("Docker-Content-Digest", key)
				w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
				if kind == "manifests" {
					var envelope struct {
						MediaType string `json:"mediaType"`
					}
					if err := json.Unmarshal(raw, &envelope); err != nil {
						http.Error(w, err.Error(), 500)
						return
					}
					w.Header().Set("Content-Type", envelope.MediaType)
				}
				w.WriteHeader(http.StatusOK)
				if req.Method != http.MethodHead {
					_, _ = w.Write(raw)
				}
			}))
			defer server.Close()
			dst, err := remote.NewRepository(strings.TrimPrefix(server.URL, "http://") + "/test")
			if err != nil {
				t.Fatal(err)
			}
			dst.PlainHTTP = true
			dst.SkipReferrersGC = true
			var stats Stats
			if err := copyGraph(ctx, src, dst, root, &stats, nil); err != nil {
				t.Fatal(err)
			}
			var parent v1.Manifest
			if err := json.Unmarshal(manifests[indexParent.Digest.String()], &parent); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(blobs[indexParent.Digest.String()], manifests[indexParent.Digest.String()]) {
				t.Fatal("manifest layer missing identical blob registration")
			}
			layer := parent.Layers[0]
			if !bytes.Equal(blobs[layer.Digest.String()], manifests[layer.Digest.String()]) {
				t.Fatal("index layer missing identical blob registration")
			}
			var index v1.Index
			if err := json.Unmarshal(manifests[layer.Digest.String()], &index); err != nil {
				t.Fatal(err)
			}
			for _, child := range index.Manifests {
				if _, ok := blobs[child.Digest.String()]; ok {
					t.Fatal("ordinary index member unnecessarily registered as blob")
				}
			}
			if stats.BlobBytesUploaded == 0 {
				t.Fatal("new layer registrations absent from accounting")
			}
			if rejected.Load() != 0 {
				t.Fatal("source envelope requested through blob endpoint")
			}
			var retry Stats
			if err := copyGraph(ctx, src, dst, root, &retry, nil); err != nil {
				t.Fatal(err)
			}
			if retry.BlobBytesUploaded != 0 || retry.ManifestBytesUploaded != 0 {
				t.Fatalf("retry uploaded existing content: %+v", retry)
			}
		})
	}
}
