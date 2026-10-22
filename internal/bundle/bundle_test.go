package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"

	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
)

type testStore map[digest.Digest][]byte

func (s testStore) Fetch(_ context.Context, d ocispec.Descriptor) (io.ReadCloser, error) {
	b, ok := s[d.Digest]
	if !ok {
		return nil, errdef.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (s testStore) Exists(_ context.Context, d ocispec.Descriptor) (bool, error) {
	_, ok := s[d.Digest]
	return ok, nil
}
func (s testStore) Push(_ context.Context, d ocispec.Descriptor, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if err := verifyBytes(d, b); err != nil {
		return err
	}
	s[d.Digest] = b
	return nil
}

func add(t *testing.T, s testStore, mt string, value any) ocispec.Descriptor {
	t.Helper()
	var raw []byte
	if b, ok := value.([]byte); ok {
		raw = b
	} else {
		var err error
		raw, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	d := ocispec.Descriptor{MediaType: mt, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	s[d.Digest] = raw
	return d
}
func artifact(t *testing.T, s testStore, body string, subject *ocispec.Descriptor) ocispec.Descriptor {
	t.Helper()
	cfg := add(t, s, ocispec.MediaTypeEmptyJSON, []byte("{}"))
	payload := add(t, s, "application/vnd.example.opaque", []byte(body))
	return add(t, s, ocispec.MediaTypeImageManifest, ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, ArtifactType: "application/vnd.example.artifact", Config: cfg, Layers: []ocispec.Descriptor{payload}, Subject: subject})
}
func metadata(root ocispec.Descriptor) Metadata {
	return Metadata{SchemaVersion: MetadataVersion, Recipe: config.Recipe{Include: []config.Selection{{Ref: "example.test/source:stable", Dependencies: dependency.Policy{Follow: "none"}, Referrers: config.Policy{Include: "all", Require: []config.RequiredReferrer{}}}}}, Roots: []Root{{Input: 0, Repository: "example.test/source", Reference: "stable", Descriptor: root}}, Origins: []Origin{{Repository: "example.test/source", Descriptor: root}}, Referrers: []Association{}, Dependencies: []DependencyResolution{{Repository: "example.test/source", Subject: root, Policy: dependency.Policy{Follow: "none"}, Outcome: "disabled", Dependencies: []ResolvedDependency{}}}}
}

func TestEnvelopeRejectsMalformedStructure(t *testing.T) {
	cases := []struct{ name, mediaType, raw string }{
		{"unknown", "application/vnd.example.manifest", `{}`},
		{"schema1", ocispec.MediaTypeImageManifest, `{"schemaVersion":1,"config":{},"layers":[]}`},
		{"duplicate", ocispec.MediaTypeImageIndex, `{"schemaVersion":2,"manifests":[],"manifests":[]}`},
		{"conflict", ocispec.MediaTypeImageIndex, `{"schemaVersion":2,"mediaType":"other","manifests":[]}`},
		{"null children", ocispec.MediaTypeImageIndex, `{"schemaVersion":2,"manifests":null}`},
		{"missing children", ocispec.MediaTypeImageIndex, `{"schemaVersion":2}`},
		{"missing config", ocispec.MediaTypeImageManifest, `{"schemaVersion":2,"layers":[]}`},
		{"null subject", ocispec.MediaTypeImageIndex, `{"schemaVersion":2,"manifests":[],"subject":null}`},
		{"conflicting structure", ocispec.MediaTypeImageIndex, `{"schemaVersion":2,"manifests":[],"layers":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := ocispec.Descriptor{MediaType: tc.mediaType, Digest: digest.FromString(tc.raw), Size: int64(len(tc.raw))}
			if _, err := ParseEnvelope(d, []byte(tc.raw)); err == nil {
				t.Fatal("accepted invalid envelope")
			}
		})
	}
	for _, mt := range []string{ocispec.MediaTypeImageIndex, DockerIndex} {
		raw := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[]}`, mt))
		d := ocispec.Descriptor{MediaType: mt, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
		if _, err := ParseEnvelope(d, raw); err != nil {
			t.Fatal(err)
		}
		raw[0] = '['
		if _, err := ParseEnvelope(d, raw); err == nil {
			t.Fatal("accepted tampered bytes")
		}
	}
}

func TestClosureChecksPayloadsAndExcludesSubject(t *testing.T) {
	s := testStore{}
	absent := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("absent"), Size: 6}
	root := artifact(t, s, "opaque bytes are not parsed", &absent)
	if err := VerifyClosure(context.Background(), s, root); err != nil {
		t.Fatal(err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(s[root.Digest], &manifest); err != nil {
		t.Fatal(err)
	}
	layer := manifest.Layers[0]
	delete(s, layer.Digest)
	if err := VerifyClosure(context.Background(), s, root); err == nil {
		t.Fatal("accepted missing layer under present root")
	}
	s[layer.Digest] = []byte("tampered")
	if err := VerifyClosure(context.Background(), s, root); err == nil {
		t.Fatal("accepted corrupted layer")
	}
	manifest.Layers[0].Data = []byte("opaque bytes are not parsed")
	root = add(t, s, ocispec.MediaTypeImageManifest, manifest)
	delete(s, layer.Digest)
	if err := VerifyClosure(context.Background(), s, root); err != nil {
		t.Fatalf("valid inline payload: %v", err)
	}
	manifest.Layers[0].Data = []byte("wrong")
	root = add(t, s, ocispec.MediaTypeImageManifest, manifest)
	if err := VerifyClosure(context.Background(), s, root); err == nil {
		t.Fatal("accepted invalid inline payload")
	}
}

func TestClosurePreservesAllPlatformsAndRejectsUnknownEnvelope(t *testing.T) {
	s := testStore{}
	amd := artifact(t, s, "amd64", nil)
	arm := artifact(t, s, "arm64", nil)
	amd.Platform = &ocispec.Platform{OS: "linux", Architecture: "amd64"}
	arm.Platform = &ocispec.Platform{OS: "linux", Architecture: "arm64"}
	root := add(t, s, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{amd, arm}})
	if err := VerifyClosure(context.Background(), s, root); err != nil {
		t.Fatal(err)
	}
	delete(s, arm.Digest)
	if err := VerifyClosure(context.Background(), s, root); err == nil {
		t.Fatal("omitted second platform")
	}
	unknown := add(t, s, "application/vnd.example.unknown", []byte("{}"))
	root = add(t, s, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []ocispec.Descriptor{unknown}})
	if err := VerifyClosure(context.Background(), s, root); err == nil {
		t.Fatal("accepted unknown index child as opaque payload")
	}
}

func TestClosureExpandsEnvelopePreviouslySeenAsOpaquePayload(t *testing.T) {
	s := testStore{}
	later := artifact(t, s, "missing structural child", nil)
	var laterManifest ocispec.Manifest
	if err := json.Unmarshal(s[later.Digest], &laterManifest); err != nil {
		t.Fatal(err)
	}
	delete(s, laterManifest.Layers[0].Digest)
	first := add(t, s, ocispec.MediaTypeImageManifest, ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, Config: later, Layers: []ocispec.Descriptor{}})
	root := add(t, s, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []ocispec.Descriptor{first, later}})
	if err := VerifyClosure(context.Background(), s, root); err == nil {
		t.Fatal("opaque first visit prevented later structural expansion")
	}
}

func TestSameBlobSupportsDifferentOpaqueMediaTypes(t *testing.T) {
	ctx := context.Background()
	s := testStore{}
	cfg := add(t, s, ocispec.MediaTypeEmptyJSON, []byte("{}"))
	layer := cfg
	layer.MediaType = "application/json"
	root := add(t, s, ocispec.MediaTypeImageManifest, ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, Config: cfg, Layers: []ocispec.Descriptor{layer}})
	if err := VerifyClosure(ctx, s, root); err != nil {
		t.Fatalf("same bytes with two opaque media types: %v", err)
	}
	snapshot, err := BuildSnapshot(ctx, s, metadata(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSnapshotGraph(ctx, s, snapshot.Descriptor); err != nil {
		t.Fatal(err)
	}
}

func TestEnvelopeInterpretationCannotHideBehindSameDigest(t *testing.T) {
	s := testStore{}
	first := artifact(t, s, "one", nil)
	conflict := first
	conflict.MediaType = DockerManifest
	root := add(t, s, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []ocispec.Descriptor{first, conflict}})
	if err := VerifyClosure(context.Background(), s, root); err == nil {
		t.Fatal("second envelope interpretation with conflicting body mediaType accepted")
	}
	// Missing body mediaType is permitted by OCI; both schema-2 manifest
	// interpretations have the same graph and must be checked and preserved.
	var manifest ocispec.Manifest
	if err := json.Unmarshal(s[first.Digest], &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.MediaType = ""
	first = add(t, s, ocispec.MediaTypeImageManifest, manifest)
	conflict = first
	conflict.MediaType = DockerManifest
	root = add(t, s, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []ocispec.Descriptor{first, conflict}})
	if err := VerifyClosure(context.Background(), s, root); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotRoundTripNestedAndDeterministic(t *testing.T) {
	ctx := context.Background()
	s := testStore{}
	root := artifact(t, s, "chart shaped payload", nil)
	ref := artifact(t, s, "prepared dependency", &root)
	m := metadata(root)
	m.Origins = append(m.Origins, Origin{Repository: "example.test/source", Descriptor: ref})
	m.Referrers = []Association{{Repository: "example.test/source", Subject: root, Descriptor: ref}}
	m.Recipe.Include[0].Referrers.Include = "none"
	m.Recipe.Include[0].Referrers.Require = []config.RequiredReferrer{{Digest: ref.Digest}}
	m.Dependencies = append(m.Dependencies, DependencyResolution{Repository: "example.test/source", Subject: ref, Policy: dependency.Policy{Follow: "none"}, Outcome: "disabled", Dependencies: []ResolvedDependency{}})
	before, _ := json.Marshal(m)
	first, err := BuildSnapshot(ctx, s, m)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(m)
	if !bytes.Equal(before, after) {
		t.Fatal("mutated caller metadata")
	}
	m.Origins[0], m.Origins[1] = m.Origins[1], m.Origins[0]
	second, err := BuildSnapshot(ctx, s, m)
	if err != nil {
		t.Fatal(err)
	}
	if first.Descriptor.Digest != second.Descriptor.Digest {
		t.Fatal("discovery order changed bundle digest")
	}
	loaded, err := LoadSnapshotGraph(ctx, s, first.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Metadata, loaded.Metadata) {
		t.Fatal("metadata differs after load")
	}
	outer := metadata(first.Descriptor)
	nested, err := BuildSnapshot(ctx, s, outer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSnapshotGraph(ctx, s, nested.Descriptor); err != nil {
		t.Fatal(err)
	}
	delete(s, root.Digest)
	if _, err := LoadSnapshotGraph(ctx, s, nested.Descriptor); err == nil {
		t.Fatal("nested bundle missing chart was accepted")
	}
}

func TestMetadataRejectsInvalidInventoryAndJSON(t *testing.T) {
	s := testStore{}
	root := artifact(t, s, "one", nil)
	m := metadata(root)
	raw, err := EncodeMetadata(m)
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]byte{
		"duplicate":     bytes.Replace(raw, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"schemaVersion":1`), 1),
		"unknown":       append([]byte(`{"unknown":0,`), raw[1:]...),
		"null":          bytes.Replace(raw, []byte(`"referrers":[]`), []byte(`"referrers":null`), 1),
		"wrong version": bytes.Replace(raw, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":2`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeMetadata(bad); err == nil {
				t.Fatal("accepted invalid JSON metadata")
			}
		})
	}
	m.Origins = nil
	if _, err := EncodeMetadata(m); err == nil {
		t.Fatal("root without origin accepted")
	}
	m = metadata(root)
	m.Roots[0].Reference = "other"
	if _, err := EncodeMetadata(m); err == nil {
		t.Fatal("root outside declared selector accepted")
	}
	m = metadata(root)
	m.Recipe.Include[0].Referrers.Require = []config.RequiredReferrer{{Digest: digest.FromString("missing")}}
	if _, err := EncodeMetadata(m); err == nil {
		t.Fatal("missing mandatory association accepted")
	}
	tags := []string{"stable", "other"}
	m = metadata(root)
	m.Recipe.Include[0].Ref = ""
	m.Recipe.Include[0].Repository = "example.test/source"
	m.Recipe.Include[0].Tags = &tags
	if _, err := EncodeMetadata(m); err == nil {
		t.Fatal("missing explicitly selected tag accepted")
	}
	m.Roots = append(m.Roots, Root{Input: 0, Repository: "example.test/source", Reference: "other", Descriptor: root})
	if _, err := EncodeMetadata(m); err != nil {
		t.Fatal(err)
	}
	tags = []string{}
	m.Roots[1].Reference = root.Digest.String()
	if _, err := EncodeMetadata(m); err == nil {
		t.Fatal("digest used as all-tags inventory tag accepted")
	}
}

func TestInPlaceBuildChecksGraphWithoutDownloadingPayload(t *testing.T) {
	ctx := context.Background()
	s := testStore{}
	root := artifact(t, s, "retained payload", nil)
	graph, err := walkClosure(ctx, s, root)
	if err != nil {
		t.Fatal(err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(s[root.Digest], &manifest); err != nil {
		t.Fatal(err)
	}
	delete(s, manifest.Layers[0].Digest)
	snapshot, err := BuildSnapshotFromGraph(ctx, s, metadata(root), graph)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyClosure(ctx, s, snapshot.Descriptor); err == nil {
		t.Fatal("source inventory substituted for offline payload verification")
	}
	delete(graph, manifest.Layers[0].Digest)
	if _, err := BuildSnapshotFromGraph(ctx, s, metadata(root), graph); err == nil {
		t.Fatal("accepted incomplete source inventory")
	}
}

func TestInspectSnapshotNeedsOnlyGeneratedMetadata(t *testing.T) {
	ctx := context.Background()
	s := testStore{}
	root := artifact(t, s, "old unavailable payload", nil)
	snapshot, err := BuildSnapshot(ctx, s, metadata(root))
	if err != nil {
		t.Fatal(err)
	}
	delete(s, root.Digest)
	inspected, err := InspectSnapshot(ctx, s, snapshot.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inspected.Metadata, snapshot.Metadata) {
		t.Fatal("inspection changed recipe snapshot")
	}
	if _, err := LoadSnapshotGraph(ctx, s, snapshot.Descriptor); err == nil {
		t.Fatal("inspection incorrectly substituted for closure verification")
	}
	var index ocispec.Index
	if err := json.Unmarshal(s[snapshot.Descriptor.Digest], &index); err != nil {
		t.Fatal(err)
	}
	delete(s, digest.Digest(index.Annotations[MetadataDigestKey]))
	if _, err := InspectSnapshot(ctx, s, snapshot.Descriptor); err == nil {
		t.Fatal("missing native metadata accepted")
	}
}

func TestSnapshotRejectsUnreachableOriginAndForgedAssociation(t *testing.T) {
	ctx := context.Background()
	s := testStore{}
	root := artifact(t, s, "root", nil)
	other := artifact(t, s, "unreachable", nil)
	m := metadata(root)
	m.Origins = append(m.Origins, Origin{Repository: "example.test/source", Descriptor: other})
	if _, err := BuildSnapshot(ctx, s, m); err == nil {
		t.Fatal("unreachable origin accepted")
	}
	m = metadata(root)
	m.Origins = append(m.Origins, Origin{Repository: "example.test/source", Descriptor: other})
	m.Referrers = []Association{{Repository: "example.test/source", Subject: root, Descriptor: other}}
	if _, err := BuildSnapshot(ctx, s, m); err == nil || !strings.Contains(err.Error(), "subject") {
		t.Fatalf("forged association: %v", err)
	}
	delete(s, root.Digest)
	if _, err := BuildSnapshot(ctx, s, metadata(root)); err == nil {
		t.Fatal("built completion root for incomplete content")
	}
}
