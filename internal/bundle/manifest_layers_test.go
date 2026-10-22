package bundle

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestClosureTraversesManifestAndIndexLayers(t *testing.T) {
	for _, mt := range []string{v1.MediaTypeImageManifest, DockerManifest, v1.MediaTypeImageIndex, DockerIndex} {
		t.Run(mt, func(t *testing.T) {
			for _, damage := range []string{"none", "missing", "corrupt"} {
				t.Run(damage, func(t *testing.T) {
					store := testStore{}
					image := artifact(t, store, "required descendant", nil)
					var manifest v1.Manifest
					if err := json.Unmarshal(store[image.Digest], &manifest); err != nil {
						t.Fatal(err)
					}
					payload := manifest.Layers[0]
					var layer v1.Descriptor
					if mt == v1.MediaTypeImageIndex || mt == DockerIndex {
						layer = add(t, store, mt, v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: mt, Manifests: []v1.Descriptor{image}})
					} else {
						manifest.MediaType = mt
						layer = add(t, store, mt, manifest)
					}
					cfg := add(t, store, v1.MediaTypeEmptyJSON, []byte("{}"))
					root := add(t, store, v1.MediaTypeImageManifest, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: "application/example.cache", Config: cfg, Layers: []v1.Descriptor{layer}})
					switch damage {
					case "missing":
						delete(store, payload.Digest)
					case "corrupt":
						store[payload.Digest] = []byte(strings.Repeat("!", int(payload.Size)))
					}
					err := VerifyClosure(context.Background(), store, root)
					if damage == "none" && err != nil {
						t.Fatal(err)
					}
					if damage != "none" && err == nil {
						t.Fatalf("accepted %s payload below %s layer", damage, mt)
					}
				})
			}
		})
	}
}

func TestClosureDoesNotInterpretUnknownLayerJSON(t *testing.T) {
	store := testStore{}
	missing := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 100}
	layer := add(t, store, "application/example.unknown", v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{missing}})
	cfg := add(t, store, v1.MediaTypeEmptyJSON, []byte("{}"))
	root := add(t, store, v1.MediaTypeImageManifest, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, Config: cfg, Layers: []v1.Descriptor{layer}, Subject: &missing})
	if err := VerifyClosure(context.Background(), store, root); err != nil {
		t.Fatalf("unknown layer content and weak subject must remain opaque: %v", err)
	}
}

func TestClosureExpandsLayerPreviouslySeenAsConfig(t *testing.T) {
	store := testStore{}
	image := artifact(t, store, "required image payload", nil)
	var manifest v1.Manifest
	if err := json.Unmarshal(store[image.Digest], &manifest); err != nil {
		t.Fatal(err)
	}
	delete(store, manifest.Layers[0].Digest)
	root := add(t, store, v1.MediaTypeImageManifest, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, Config: image, Layers: []v1.Descriptor{image}})
	if err := VerifyClosure(context.Background(), store, root); err == nil {
		t.Fatal("opaque config visit hid the same digest's manifest layer dependencies")
	}
}

func TestClosureRejectsMalformedRecognizedManifestLayer(t *testing.T) {
	store := testStore{}
	layer := add(t, store, v1.MediaTypeImageManifest, []byte(`{"schemaVersion":1,"config":{},"layers":[]}`))
	cfg := add(t, store, v1.MediaTypeEmptyJSON, []byte("{}"))
	root := add(t, store, v1.MediaTypeImageManifest, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, Config: cfg, Layers: []v1.Descriptor{layer}})
	if err := VerifyClosure(context.Background(), store, root); err == nil {
		t.Fatal("accepted malformed recognized manifest layer despite valid payload digest")
	}
}
