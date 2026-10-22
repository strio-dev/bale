package transport

import (
	"context"
	"errors"
	"fmt"
	"os"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/deliveryfile"
	"oras.land/oras-go/v2/content/oci"
)

// UpdateDelivery carries delivery paths between captured remote bundles. Each
// operand is pinned once; identical reference strings reuse the same snapshot.
// Only envelopes and interpreted metadata are read, without refreshing source
// selectors, discovering referrers, executing plugins, or checking payloads.
func (s *Service) UpdateDelivery(ctx context.Context, beforeRef, afterRef string, file deliveryfile.File) (DeliveryUpdateResult, error) {
	var before deliveryUpdateSnapshot
	load := func(reference string) (deliveryUpdateSnapshot, error) {
		root, err := s.Resolve(ctx, reference)
		if err != nil {
			return deliveryUpdateSnapshot{}, err
		}
		return s.deliveryUpdateRoot(ctx, root)
	}
	return deliveryUpdateInputs(ctx, beforeRef, afterRef, file, func() (deliveryUpdateSnapshot, error) {
		var err error
		before, err = load(beforeRef)
		return before, err
	}, func() (deliveryUpdateSnapshot, error) {
		if beforeRef == afterRef {
			return before, nil
		}
		return load(afterRef)
	})
}

// UpdateDeliveryLayouts reads completed single-root OCI bundle layouts without
// writes or network access. Missing opaque application payloads are permitted;
// captured envelopes, dependency declarations and nested metadata are verified.
func (s *Service) UpdateDeliveryLayouts(ctx context.Context, beforePath, afterPath string, file deliveryfile.File) (DeliveryUpdateResult, error) {
	load := func(path string) (deliveryUpdateSnapshot, error) {
		inspection, err := s.inspectLayout(ctx, path, bundle.InspectSnapshotGraph)
		if err != nil {
			return deliveryUpdateSnapshot{}, err
		}
		return deliveryUpdateMetadata(ctx, Root{Descriptor: inspection.Descriptor, Store: oci.NewStorageFromFS(os.DirFS(path))})
	}
	return deliveryUpdateInputs(ctx, beforePath, afterPath, file,
		func() (deliveryUpdateSnapshot, error) { return load(beforePath) },
		func() (deliveryUpdateSnapshot, error) { return load(afterPath) })
}

// UpdateDeliveryRoots carries paths between pinned bundles in caller-owned
// stores. Stores remain open and unchanged. Nested captured inventories are
// included; opaque payload availability and integrity are not checked.
func (s *Service) UpdateDeliveryRoots(ctx context.Context, beforeRoot, afterRoot Root, file deliveryfile.File) (DeliveryUpdateResult, error) {
	return deliveryUpdateInputs(ctx, diffRootName(beforeRoot), diffRootName(afterRoot), file,
		func() (deliveryUpdateSnapshot, error) { return s.deliveryUpdateRoot(ctx, beforeRoot) },
		func() (deliveryUpdateSnapshot, error) { return s.deliveryUpdateRoot(ctx, afterRoot) })
}

type deliveryUpdateSnapshot struct {
	descriptor v1.Descriptor
	metadata   []bundle.Metadata
}

func (s *Service) deliveryUpdateRoot(ctx context.Context, root Root) (deliveryUpdateSnapshot, error) {
	if _, err := s.inspectRoot(ctx, root, bundle.InspectSnapshotGraph); err != nil {
		return deliveryUpdateSnapshot{}, err
	}
	return deliveryUpdateMetadata(ctx, root)
}

func deliveryUpdateMetadata(ctx context.Context, root Root) (deliveryUpdateSnapshot, error) {
	metadata, err := collectBundleMetadata(ctx, attachmentSource{root.Store}, root.Descriptor, bundle.InspectSnapshot)
	if err != nil {
		kind := ErrIntegrity
		var sourceErr *attachmentSourceError
		if errors.As(err, &sourceErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			kind = nil
		}
		return deliveryUpdateSnapshot{}, operationError("read delivery metadata", root.Repository, root.Descriptor, kind, err)
	}
	return deliveryUpdateSnapshot{descriptor: root.Descriptor, metadata: metadata}, nil
}

func deliveryUpdateInputs(ctx context.Context, beforeName, afterName string, file deliveryfile.File, loadBefore, loadAfter func() (deliveryUpdateSnapshot, error)) (DeliveryUpdateResult, error) {
	if err := ctx.Err(); err != nil {
		return DeliveryUpdateResult{}, fmt.Errorf("update delivery before %q: %w", beforeName, err)
	}
	before, err := loadBefore()
	if err != nil {
		return DeliveryUpdateResult{}, fmt.Errorf("update delivery before %q: %w", beforeName, err)
	}
	if err := ctx.Err(); err != nil {
		return DeliveryUpdateResult{}, fmt.Errorf("update delivery after %q: %w", afterName, err)
	}
	after, err := loadAfter()
	if err != nil {
		return DeliveryUpdateResult{}, fmt.Errorf("update delivery after %q: %w", afterName, err)
	}
	if err := ctx.Err(); err != nil {
		return DeliveryUpdateResult{}, fmt.Errorf("update delivery: %w", err)
	}
	result, err := updateDelivery(before.descriptor, after.descriptor, before.metadata, after.metadata, file)
	if canceled := ctx.Err(); canceled != nil {
		return DeliveryUpdateResult{}, fmt.Errorf("update delivery: %w", canceled)
	}
	if err != nil {
		if !errors.Is(err, ErrConflict) {
			result = DeliveryUpdateResult{}
		}
		return result, operationError("update delivery", "", before.descriptor, ErrInvalid, err)
	}
	return result, nil
}
