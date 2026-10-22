package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

// BuildSnapshot adds the small native metadata envelope to already-stored
// artifacts. Every selected graph is checked before publishing the bundle root.
func BuildSnapshot(ctx context.Context, store content.Storage, m Metadata) (Snapshot, error) {
	return buildSnapshot(ctx, store, m, nil)
}

// BuildSnapshotFromGraph supports in-place registry capture. The caller must
// have validated every inventory descriptor against its source repository
// (including HEAD checks of opaque payloads). This function verifies envelope
// bytes and every structural edge against that inventory, without downloading
// payloads that the registry already stores. It never replaces full offline
// verification by VerifyClosure or LoadSnapshotGraph.
func BuildSnapshotFromGraph(ctx context.Context, store content.Storage, m Metadata, inventory map[digest.Digest]ocispec.Descriptor) (Snapshot, error) {
	if inventory == nil {
		return Snapshot{}, fmt.Errorf("verified source inventory is required")
	}
	return buildSnapshot(ctx, store, m, inventory)
}

func buildSnapshot(ctx context.Context, store content.Storage, m Metadata, inventory map[digest.Digest]ocispec.Descriptor) (Snapshot, error) {
	raw, err := EncodeMetadata(m)
	if err != nil {
		return Snapshot{}, err
	}
	normalized, err := DecodeMetadata(raw)
	if err != nil {
		return Snapshot{}, err
	}
	children := map[digest.Digest]ocispec.Descriptor{}
	for _, r := range normalized.Roots {
		children[r.Descriptor.Digest] = r.Descriptor
	}
	for _, a := range normalized.Referrers {
		children[a.Descriptor.Digest] = a.Descriptor
	}
	for _, resolution := range normalized.Dependencies {
		for _, dep := range resolution.Dependencies {
			children[dep.Descriptor.Digest] = dep.Descriptor
		}
	}
	for _, p := range normalized.Plugins {
		children[p.Descriptor.Digest] = p.Descriptor
	}
	reachable := map[digest.Digest]ocispec.Descriptor{}
	for _, d := range children {
		graph, err := walkGraph(ctx, store, d, inventory)
		if err != nil {
			return Snapshot{}, err
		}
		for dgst, desc := range graph {
			if old, ok := reachable[dgst]; ok && !sameDescriptor(old, desc) {
				return Snapshot{}, fmt.Errorf("conflicting graph descriptor %s", dgst)
			}
			reachable[dgst] = desc
		}
	}
	if err := validateCapturedGraph(ctx, store, normalized, children, reachable); err != nil {
		return Snapshot{}, err
	}
	metadataBlob, err := pushBytes(ctx, store, MetadataMediaType, raw)
	if err != nil {
		return Snapshot{}, err
	}
	empty, err := pushBytes(ctx, store, ocispec.MediaTypeEmptyJSON, []byte("{}"))
	if err != nil {
		return Snapshot{}, err
	}
	manifest := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, ArtifactType: MetadataArtifactType, Config: empty, Layers: []ocispec.Descriptor{metadataBlob}}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return Snapshot{}, err
	}
	metadataDesc, err := pushBytes(ctx, store, ocispec.MediaTypeImageManifest, manifestBytes)
	if err != nil {
		return Snapshot{}, err
	}
	metadataDesc.ArtifactType = MetadataArtifactType
	ordered := make([]ocispec.Descriptor, 0, len(children))
	for _, d := range children {
		ordered = append(ordered, d)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Digest < ordered[j].Digest })
	index := ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, ArtifactType: BundleArtifactType, Manifests: append([]ocispec.Descriptor{metadataDesc}, ordered...), Annotations: map[string]string{MetadataDigestKey: metadataDesc.Digest.String()}}
	indexBytes, err := json.Marshal(index)
	if err != nil {
		return Snapshot{}, err
	}
	root, err := pushBytes(ctx, store, ocispec.MediaTypeImageIndex, indexBytes)
	if err != nil {
		return Snapshot{}, err
	}
	root.ArtifactType = BundleArtifactType
	return Snapshot{Descriptor: root, Metadata: normalized}, nil
}

func pushBytes(ctx context.Context, store content.Storage, mediaType string, raw []byte) (ocispec.Descriptor, error) {
	d := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	if err := store.Push(ctx, d, bytes.NewReader(raw)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return ocispec.Descriptor{}, err
	}
	return d, nil
}

// LoadSnapshotFromGraph validates a snapshot against capture's source-verified
// descriptor inventory. Like BuildSnapshotFromGraph, it checks structural and
// captured metadata relationships without rereading opaque application payloads.
// It does not replace full payload hashing for offline export or delivery.
func LoadSnapshotFromGraph(ctx context.Context, store content.ReadOnlyStorage, root ocispec.Descriptor, inventory map[digest.Digest]ocispec.Descriptor) (Snapshot, error) {
	if inventory == nil {
		return Snapshot{}, fmt.Errorf("verified source inventory is required")
	}
	reachable, err := walkGraph(ctx, store, root, inventory)
	if err != nil {
		return Snapshot{}, err
	}
	return readSnapshot(ctx, store, root, reachable)
}

// InspectSnapshot reads only the bundle index and its generated metadata
// envelope. Latest capture uses the validated recipe without requiring old
// payloads to remain available. This does not prove closure, source coordinates
// or referrer subjects; use LoadSnapshotGraph before trusting an offline transfer.
func InspectSnapshot(ctx context.Context, store content.ReadOnlyStorage, root ocispec.Descriptor) (Snapshot, error) {
	return readSnapshot(ctx, store, root, nil)
}

func readSnapshot(ctx context.Context, store content.ReadOnlyStorage, root ocispec.Descriptor, reachable map[digest.Digest]ocispec.Descriptor) (Snapshot, error) {
	if root.MediaType != ocispec.MediaTypeImageIndex {
		return Snapshot{}, fmt.Errorf("bundle root must be an OCI index")
	}
	raw, err := fetchBytes(ctx, store, root)
	if err != nil {
		return Snapshot{}, err
	}
	if _, err := ParseEnvelope(root, raw); err != nil {
		return Snapshot{}, err
	}
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return Snapshot{}, err
	}
	if index.ArtifactType != BundleArtifactType {
		return Snapshot{}, fmt.Errorf("not a native Bale bundle")
	}
	metadataDigest, err := digest.Parse(index.Annotations[MetadataDigestKey])
	if err != nil {
		return Snapshot{}, fmt.Errorf("bundle metadata digest annotation: %w", err)
	}
	var metadataDesc ocispec.Descriptor
	children := map[digest.Digest]ocispec.Descriptor{}
	count := 0
	for _, d := range index.Manifests {
		if _, ok := children[d.Digest]; ok {
			return Snapshot{}, fmt.Errorf("duplicate direct bundle child %s", d.Digest)
		}
		children[d.Digest] = d
		if d.Digest == metadataDigest {
			metadataDesc = d
			count++
		}
	}
	if count != 1 || metadataDesc.MediaType != ocispec.MediaTypeImageManifest {
		return Snapshot{}, fmt.Errorf("bundle must contain exactly one metadata manifest selected by annotation")
	}
	raw, err = fetchBytes(ctx, store, metadataDesc)
	if err != nil {
		return Snapshot{}, err
	}
	if _, err := ParseEnvelope(metadataDesc, raw); err != nil {
		return Snapshot{}, err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Snapshot{}, err
	}
	if manifest.ArtifactType != MetadataArtifactType || manifest.Subject != nil || len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != MetadataMediaType || manifest.Config.MediaType != ocispec.MediaTypeEmptyJSON {
		return Snapshot{}, fmt.Errorf("invalid Bale metadata manifest")
	}
	empty, err := fetchBytes(ctx, store, manifest.Config)
	if err != nil {
		return Snapshot{}, err
	}
	if !bytes.Equal(empty, []byte("{}")) {
		return Snapshot{}, fmt.Errorf("metadata config must be the OCI empty JSON object")
	}
	raw, err = fetchBytes(ctx, store, manifest.Layers[0])
	if err != nil {
		return Snapshot{}, err
	}
	m, err := DecodeMetadata(raw)
	if err != nil {
		return Snapshot{}, err
	}
	delete(children, metadataDigest)
	if err := validateMembership(m, children); err != nil {
		return Snapshot{}, err
	}
	if reachable != nil {
		if err := validateCapturedGraph(ctx, store, m, children, reachable); err != nil {
			return Snapshot{}, err
		}
	}
	return Snapshot{Descriptor: root, Metadata: m}, nil
}

func validateCapturedGraph(ctx context.Context, store content.ReadOnlyStorage, m Metadata, children, reachable map[digest.Digest]ocispec.Descriptor) error {
	if err := validateMembership(m, children); err != nil {
		return err
	}
	for _, r := range m.Roots {
		raw, err := fetchBytes(ctx, store, r.Descriptor)
		if err != nil {
			return err
		}
		if _, err := ParseEnvelope(r.Descriptor, raw); err != nil {
			return err
		}
	}
	for _, o := range m.Origins {
		actual, ok := reachable[o.Descriptor.Digest]
		if !ok || !sameDescriptor(o.Descriptor, actual) {
			return fmt.Errorf("origin %s is not reachable with its captured descriptor", o.Descriptor.Digest)
		}
		// Metadata may supply a different envelope interpretation of the same
		// bytes. Validate that interpretation rather than silently trusting the
		// first descriptor encountered in the structural graph.
		raw, err := fetchBytes(ctx, store, o.Descriptor)
		if err != nil {
			return err
		}
		if _, err := ParseEnvelope(o.Descriptor, raw); err != nil {
			return err
		}
	}
	for _, a := range m.Referrers {
		subjectRaw, err := fetchBytes(ctx, store, a.Subject)
		if err != nil {
			return err
		}
		if _, err := ParseEnvelope(a.Subject, subjectRaw); err != nil {
			return err
		}
		raw, err := fetchBytes(ctx, store, a.Descriptor)
		if err != nil {
			return err
		}
		env, err := ParseEnvelope(a.Descriptor, raw)
		if err != nil {
			return err
		}
		if env.Subject == nil || !sameDescriptor(*env.Subject, a.Subject) {
			return fmt.Errorf("referrer %s has a different subject than its captured association", a.Descriptor.Digest)
		}
	}
	return validateCapturedDependencies(ctx, store, m)
}

func validateMembership(m Metadata, children map[digest.Digest]ocispec.Descriptor) error {
	expected := map[digest.Digest]ocispec.Descriptor{}
	for _, r := range m.Roots {
		expected[r.Descriptor.Digest] = r.Descriptor
	}
	for _, a := range m.Referrers {
		expected[a.Descriptor.Digest] = a.Descriptor
	}
	for _, resolution := range m.Dependencies {
		for _, dep := range resolution.Dependencies {
			expected[dep.Descriptor.Digest] = dep.Descriptor
		}
	}
	for _, p := range m.Plugins {
		expected[p.Descriptor.Digest] = p.Descriptor
	}
	if len(expected) != len(children) {
		return fmt.Errorf("bundle membership differs from captured roots, referrers, dependencies and plugins")
	}
	for dgst, d := range expected {
		actual, ok := children[dgst]
		if !ok || !sameDescriptor(d, actual) {
			return fmt.Errorf("captured member %s is not a matching direct bundle child", dgst)
		}
	}
	return nil
}
