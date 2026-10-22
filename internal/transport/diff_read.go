package transport

import (
	"context"
	"fmt"

	"github.com/strio-dev/bale/internal/bundle"
)

// Diff pins each remote operand once and compares captured metadata. Identical
// reference strings reuse one inspection, including one mutable-tag resolution.
// Opaque application payloads are neither read nor checked for availability.
func (s *Service) Diff(ctx context.Context, beforeRef, afterRef string) (DiffResult, error) {
	var before InspectResult
	loadBefore := func() (InspectResult, error) {
		root, err := s.Resolve(ctx, beforeRef)
		if err != nil {
			return InspectResult{}, err
		}
		before, err = s.inspectRoot(ctx, root, bundle.InspectSnapshotGraph)
		return before, err
	}
	loadAfter := func() (InspectResult, error) {
		if beforeRef == afterRef {
			return before, nil
		}
		root, err := s.Resolve(ctx, afterRef)
		if err != nil {
			return InspectResult{}, err
		}
		return s.inspectRoot(ctx, root, bundle.InspectSnapshotGraph)
	}
	return diffInputs(ctx, beforeRef, afterRef, loadBefore, loadAfter)
}

// DiffLayouts compares completed single-root OCI bundle layouts without writes.
// It validates envelopes and interpreted metadata, but not opaque payloads.
func (s *Service) DiffLayouts(ctx context.Context, beforePath, afterPath string) (DiffResult, error) {
	return diffInputs(ctx, beforePath, afterPath,
		func() (InspectResult, error) { return s.inspectLayout(ctx, beforePath, bundle.InspectSnapshotGraph) },
		func() (InspectResult, error) { return s.inspectLayout(ctx, afterPath, bundle.InspectSnapshotGraph) })
}

// DiffRoots compares pinned bundles in caller-owned stores. It does not close
// stores, discover referrers, refresh source selectors, run plugins, or publish.
func (s *Service) DiffRoots(ctx context.Context, beforeRoot, afterRoot Root) (DiffResult, error) {
	return diffInputs(ctx, diffRootName(beforeRoot), diffRootName(afterRoot),
		func() (InspectResult, error) { return s.inspectRoot(ctx, beforeRoot, bundle.InspectSnapshotGraph) },
		func() (InspectResult, error) { return s.inspectRoot(ctx, afterRoot, bundle.InspectSnapshotGraph) })
}

func diffRootName(root Root) string {
	if root.Repository == "" {
		return root.Descriptor.Digest.String()
	}
	return root.Repository + "@" + root.Descriptor.Digest.String()
}

func diffInputs(ctx context.Context, beforeName, afterName string, loadBefore, loadAfter func() (InspectResult, error)) (DiffResult, error) {
	if err := ctx.Err(); err != nil {
		return DiffResult{}, fmt.Errorf("diff before %q: %w", beforeName, err)
	}
	before, err := loadBefore()
	if err != nil {
		return DiffResult{}, fmt.Errorf("diff before %q: %w", beforeName, err)
	}
	if err := ctx.Err(); err != nil {
		return DiffResult{}, fmt.Errorf("diff after %q: %w", afterName, err)
	}
	after, err := loadAfter()
	if err != nil {
		return DiffResult{}, fmt.Errorf("diff after %q: %w", afterName, err)
	}
	if err := ctx.Err(); err != nil {
		return DiffResult{}, fmt.Errorf("diff: %w", err)
	}
	return compareSnapshots(before, after), nil
}
