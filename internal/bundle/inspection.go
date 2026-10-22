package bundle

import (
	"context"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
)

// LoadSnapshotGraph verifies the complete strong closure once and validates the
// captured metadata at every native bundle boundary. Each nested snapshot is
// checked against its own reachable graph, never the enclosing inventory.
// Opaque application payloads are streamed once and are not buffered or reread;
// envelopes and interpreted Bale metadata may be read again during validation.
func LoadSnapshotGraph(ctx context.Context, store content.ReadOnlyStorage, root ocispec.Descriptor) (Snapshot, error) {
	inventory, err := walkClosure(ctx, store, root)
	if err != nil {
		return Snapshot{}, err
	}
	return validateSnapshotGraph(ctx, store, root, inventory)
}

// InspectSnapshotGraph verifies envelopes and interpreted Bale metadata at every
// native bundle boundary, including scoped membership, origins, subjects and
// dependency declarations. It neither reads nor checks the existence of opaque
// application payloads, and therefore does not prove their availability or
// integrity. Transfers and full inspection must use LoadSnapshotGraph instead.
func InspectSnapshotGraph(ctx context.Context, store content.ReadOnlyStorage, root ocispec.Descriptor) (Snapshot, error) {
	inventory, err := walkGraphWithPayloads(ctx, store, root, nil, false)
	if err != nil {
		return Snapshot{}, err
	}
	return validateSnapshotGraph(ctx, store, root, inventory)
}

func validateSnapshotGraph(ctx context.Context, store content.ReadOnlyStorage, root ocispec.Descriptor, inventory map[digest.Digest]ocispec.Descriptor) (Snapshot, error) {
	snapshot, err := readSnapshot(ctx, store, root, inventory)
	if err != nil {
		return Snapshot{}, err
	}
	seen := map[string]bool{}
	var walk func(ocispec.Descriptor, bool) error
	walk = func(d ocispec.Descriptor, isRoot bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := d.Digest.String() + "\x00" + d.MediaType + "\x00" + d.ArtifactType
		if seen[key] {
			return nil
		}
		seen[key] = true
		raw, err := fetchBytes(ctx, store, d)
		if err != nil {
			return err
		}
		envelope, err := ParseEnvelope(d, raw)
		if err != nil {
			return err
		}
		if !isRoot && envelope.ArtifactType == BundleArtifactType {
			// Reuse the caller's payload policy while validating each nested
			// snapshot against its own structural graph rather than siblings.
			reachable, err := walkGraph(ctx, store, d, inventory)
			if err != nil {
				return err
			}
			if _, err := readSnapshot(ctx, store, d, reachable); err != nil {
				return err
			}
		}
		for i, child := range envelope.Children {
			if IsEnvelopeChild(d, i, child) {
				if err := walk(child, false); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, true); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}
