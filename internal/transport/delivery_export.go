package transport

import (
	"context"
	"errors"
	"fmt"
	"os"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/deliveryfile"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"
)

func defaultDelivery(root v1.Descriptor, metadata []bundle.Metadata) (deliveryfile.File, error) {
	file := deliveryfile.File{Bundle: root.Digest, Artifacts: []deliveryfile.Artifact{}}
	seen := map[string]bool{}
	for _, m := range metadata {
		for _, origin := range m.Origins {
			ref := origin.Repository + "@" + origin.Descriptor.Digest.String()
			if seen[ref] {
				continue
			}
			seen[ref] = true
			repository, err := registry.ParseReference(origin.Repository)
			if err != nil {
				return deliveryfile.File{}, err
			}
			file.Artifacts = append(file.Artifacts, deliveryfile.Artifact{Ref: ref, Path: repository.Repository})
		}
	}
	return deliveryfile.Normalize(file)
}

func (s *Service) ExportDelivery(ctx context.Context, reference string) (deliveryfile.File, error) {
	root, err := s.Resolve(ctx, reference)
	if err != nil {
		return deliveryfile.File{}, err
	}
	if _, err := s.inspectRoot(ctx, root, bundle.InspectSnapshotGraph); err != nil {
		return deliveryfile.File{}, err
	}
	return deliveryFromVerified(ctx, attachmentSource{root.Store}, root.Descriptor)
}

func (s *Service) ExportDeliveryLayout(ctx context.Context, path string) (deliveryfile.File, error) {
	inspection, err := s.InspectLayout(ctx, path)
	if err != nil {
		return deliveryfile.File{}, err
	}
	return deliveryFromVerified(ctx, attachmentSource{oci.NewStorageFromFS(os.DirFS(path))}, inspection.Descriptor)
}

func (s *Service) ExportDeliveryRoot(ctx context.Context, root Root) (deliveryfile.File, error) {
	if _, err := s.InspectRoot(ctx, root); err != nil {
		return deliveryfile.File{}, err
	}
	return deliveryFromVerified(ctx, attachmentSource{root.Store}, root.Descriptor)
}

func deliveryFromVerified(ctx context.Context, store content.ReadOnlyStorage, root v1.Descriptor) (deliveryfile.File, error) {
	// The caller validated captured metadata at every native bundle boundary.
	// Read those inventories again without reading opaque application payloads.
	metadata, err := collectBundleMetadata(ctx, store, root, bundle.InspectSnapshot)
	if err != nil {
		kind := ErrIntegrity
		var sourceErr *attachmentSourceError
		if errors.As(err, &sourceErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			kind = nil
		}
		return deliveryfile.File{}, operationError("export delivery", "", root, kind, err)
	}
	file, err := defaultDelivery(root, metadata)
	if err != nil {
		return deliveryfile.File{}, operationError("export delivery", "", root, ErrIntegrity, err)
	}
	return file, nil
}

// deliveryFromCapture uses the source descriptors already verified by capture.
// Only interpreted Bale metadata is fetched; application payloads stay streamed
// sources and mutable tags are never resolved again.
func deliveryFromCapture(ctx context.Context, g *graph, snapshot bundle.Snapshot) (deliveryfile.File, error) {
	store := captureDeliveryStore{g}
	load := func(ctx context.Context, source content.ReadOnlyStorage, descriptor v1.Descriptor) (bundle.Snapshot, error) {
		if descriptor.Digest == snapshot.Descriptor.Digest {
			return snapshot, nil
		}
		return bundle.LoadSnapshotFromGraph(ctx, source, descriptor, g.nodes)
	}
	metadata, err := collectBundleMetadata(ctx, store, snapshot.Descriptor, load)
	if err != nil {
		return deliveryfile.File{}, err
	}
	return defaultDelivery(snapshot.Descriptor, metadata)
}

type captureDeliveryStore struct{ *graph }

func (s captureDeliveryStore) Exists(ctx context.Context, d v1.Descriptor) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if known, ok := s.nodes[d.Digest]; ok {
		if known.Size != d.Size {
			return false, fmt.Errorf("%w: conflicting captured size for %s", ErrIntegrity, d.Digest)
		}
		return true, nil
	}
	return s.data.Exists(ctx, d)
}
