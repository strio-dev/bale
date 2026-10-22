package transport

import (
	"context"
	"encoding/json"
	"errors"
	specs "github.com/opencontainers/image-spec/specs-go"
	"io"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func TestInspectVerifiesNestedBundleMetadata(t *testing.T) {
	store := memory.New()
	member := object(t, store, "member")
	inner := snapshotFor(t, store, []string{"example.test/member"}, []v1.Descriptor{member})
	raw, err := content.FetchAll(ctx, store, inner.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	// The nested graph is byte-valid and complete, but its Bale metadata selection
	// is false. Outer closure verification alone cannot detect this corruption.
	index.Annotations[bundle.MetadataDigestKey] = digest.FromString("not selected metadata").String()
	raw, err = json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	invalidInner := put(t, store, v1.MediaTypeImageIndex, raw)
	invalidInner.ArtifactType = bundle.BundleArtifactType
	outer := snapshotFor(t, store, []string{"example.test/inner"}, []v1.Descriptor{invalidInner})
	if err := bundle.VerifyClosure(ctx, store, outer.Descriptor); err != nil {
		t.Fatalf("fixture must have a hash-valid closure: %v", err)
	}
	service, err := NewService(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.InspectRoot(ctx, Root{Descriptor: outer.Descriptor, Store: store}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("nested invalid metadata: %v", err)
	}
	if _, err := service.inspectRoot(ctx, Root{Descriptor: outer.Descriptor, Store: store}, bundle.InspectSnapshotGraph); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("metadata-only inspection accepted nested invalid metadata: %v", err)
	}
}

func TestInspectEmptyDeclarationsAndReadOnlyCaller(t *testing.T) {
	store := memory.New()
	root := object(t, store, "member")
	snapshot := snapshotFor(t, store, []string{"example.test/member"}, []v1.Descriptor{root})
	service, err := NewService(Options{})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := service.InspectRoot(ctx, Root{Descriptor: snapshot.Descriptor, Store: readOnlyInspectStore{store}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(inspected)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["declarations"]) != "[]" {
		t.Fatalf("empty declarations not an array: %s", raw)
	}
	if _, err := content.FetchAll(ctx, store, root); err != nil {
		t.Fatalf("caller store no longer available: %v", err)
	}
}

type readOnlyInspectStore struct{ content.ReadOnlyStorage }

func TestInspectDeduplicatesSelectedDeclarationAcrossPolicies(t *testing.T) {
	r, repo, service := testRegistry(t)
	subject := object(t, r.store, "subject")
	r.tags["root"] = subject
	declaration := dependencyDeclaration(t, r, subject, "production")
	selections := []config.Selection{}
	for _, selector := range []*dependency.Selector{nil, {Digest: declaration.Digest}, {Context: "production"}} {
		input := dependencySelection(repo + ":root")
		input.Dependencies.Select = selector
		selections = append(selections, input)
	}
	captured, snapshot, err := service.capture(ctx, config.Recipe{Include: selections})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Metadata.Dependencies) != 3 {
		t.Fatalf("fixture needs three policy observations: %+v", snapshot.Metadata.Dependencies)
	}
	store := memory.New()
	if err := copyGraph(ctx, captured, store, snapshot.Descriptor, &Stats{}, nil); err != nil {
		t.Fatal(err)
	}
	result, err := service.InspectRoot(ctx, Root{Descriptor: snapshot.Descriptor, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Declarations) != 1 || result.Declarations[0].Descriptor.Digest != declaration.Digest {
		t.Fatalf("duplicate declaration: %+v", result.Declarations)
	}
}

// Counting only application content distinguishes streamed opaque bytes from
// envelopes and Bale metadata, which inspection must interpret separately.
func TestInspectReadsOpaquePayloadsOnceAcrossSharedNestedBundles(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "ordinary"
		if nested {
			name = "shared nested"
		}
		t.Run(name, func(t *testing.T) {
			store := memory.New()
			config := put(t, store, "application/example.config", []byte("opaque application configuration"))
			payload := put(t, store, "application/example.payload", []byte("opaque application payload"))
			raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{payload}})
			if err != nil {
				t.Fatal(err)
			}
			member := put(t, store, v1.MediaTypeImageManifest, raw)
			snapshot := snapshotFor(t, store, []string{"example.test/member"}, []v1.Descriptor{member})
			if nested {
				other := snapshotFor(t, store, []string{"other.test/member"}, []v1.Descriptor{member})
				middle := snapshotFor(t, store, []string{"example.test/inner", "other.test/inner"}, []v1.Descriptor{snapshot.Descriptor, other.Descriptor})
				snapshot = snapshotFor(t, store, []string{"example.test/middle", "example.test/direct"}, []v1.Descriptor{middle.Descriptor, member})
			}
			counting := &countingInspectStore{ReadOnlyStorage: store, fetches: map[digest.Digest]int{}}
			service, err := NewService(Options{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.InspectRoot(ctx, Root{Descriptor: snapshot.Descriptor, Store: counting}); err != nil {
				t.Fatal(err)
			}
			for _, d := range []v1.Descriptor{config, payload} {
				if counting.fetches[d.Digest] != 1 {
					t.Fatalf("opaque %s fetched %d times", d.Digest, counting.fetches[d.Digest])
				}
			}
		})
	}
}

type countingInspectStore struct {
	content.ReadOnlyStorage
	fetches map[digest.Digest]int
}

func (s *countingInspectStore) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	s.fetches[d.Digest]++
	return s.ReadOnlyStorage.Fetch(ctx, d)
}

func TestInspectNestedOriginsRequireNestedReachability(t *testing.T) {
	store := memory.New()
	member := object(t, store, "inner member")
	outsider := object(t, store, "outer-only member")
	inner := snapshotFor(t, store, []string{"example.test/member"}, []v1.Descriptor{member})
	// Rebuild the nested metadata envelope with a false origin whose bytes exist
	// only in a sibling branch of the outer bundle. Every digest still matches.
	raw, err := content.FetchAll(ctx, store, inner.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	for i, d := range index.Manifests {
		if d.Digest.String() != index.Annotations[bundle.MetadataDigestKey] {
			continue
		}
		raw, err = content.FetchAll(ctx, store, d)
		if err != nil {
			t.Fatal(err)
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		metadata := inner.Metadata
		metadata.Origins = append(metadata.Origins, bundle.Origin{Repository: "example.test/outsider", Descriptor: outsider})
		raw, err = bundle.EncodeMetadata(metadata)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Layers[0] = put(t, store, bundle.MetadataMediaType, raw)
		raw, err = json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		replacement := put(t, store, v1.MediaTypeImageManifest, raw)
		replacement.ArtifactType = bundle.MetadataArtifactType
		index.Manifests[i] = replacement
		index.Annotations[bundle.MetadataDigestKey] = replacement.Digest.String()
		break
	}
	raw, err = json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	invalidInner := put(t, store, v1.MediaTypeImageIndex, raw)
	invalidInner.ArtifactType = bundle.BundleArtifactType
	outer := snapshotFor(t, store, []string{"example.test/inner", "example.test/outsider"}, []v1.Descriptor{invalidInner, outsider})
	if err := bundle.VerifyClosure(ctx, store, outer.Descriptor); err != nil {
		t.Fatalf("fixture closure: %v", err)
	}
	service, err := NewService(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.InspectRoot(ctx, Root{Descriptor: outer.Descriptor, Store: store}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("nested origin escaped scope: %v", err)
	}
	if _, err := service.inspectRoot(ctx, Root{Descriptor: outer.Descriptor, Store: store}, bundle.InspectSnapshotGraph); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("metadata-only inspection let nested origin escape scope: %v", err)
	}
}
