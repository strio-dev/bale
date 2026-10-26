package transport

import (
	"context"
	"fmt"

	"github.com/strio-dev/bale/internal/bundle"
	"oras.land/oras-go/v2/content"
)

// PullInto retrieves a complete bundle into caller-owned storage. The caller
// publishes its retained root only after this method succeeds and coordinates
// concurrent garbage collection. Failed calls may leave reusable CAS content.
func (s *Service) PullInto(ctx context.Context, reference string, target content.Storage, mode PullMode, options PullOptions) (result PullResult, retErr error) {
	result = PullResult{Mode: mode, Stats: Stats{Accounting: statsAccounting}}
	defer func() {
		if retErr != nil {
			retErr = operationError("pull", reference, result.Descriptor, nil, retErr)
		}
	}()
	if target == nil {
		return result, operationError("pull", reference, result.Descriptor, ErrInvalid, fmt.Errorf("target storage is required"))
	}
	if mode != PullLocked && mode != PullLatest {
		return result, fmt.Errorf("%w: pull mode must be locked or latest", ErrInvalid)
	}
	root, err := s.Resolve(ctx, reference)
	if err != nil {
		return result, err
	}
	var source content.Fetcher = root.Store
	var captured *graph
	if mode == PullLatest {
		snapshot, err := bundle.InspectSnapshot(ctx, root.Store, root.Descriptor)
		if err != nil {
			return result, err
		}
		if recipeHasPlugins(snapshot.Metadata.Recipe) && !options.AllowPlugins {
			return result, fmt.Errorf("%w: latest refresh includes plugins; explicit execution permission is required", ErrInvalid)
		}
		captured, snapshot, err = s.capture(ctx, snapshot.Metadata.Recipe)
		if err != nil {
			return result, err
		}
		root.Descriptor = snapshot.Descriptor
		source = captured
	}
	result.Descriptor = root.Descriptor
	defer func() {
		result.Stats.BlobBytesDownloaded = result.Stats.BlobBytesUploaded
		result.Stats.ManifestBytesDownloaded = result.Stats.ManifestBytesUploaded
		result.Stats.BlobBytesUploaded, result.Stats.ManifestBytesUploaded = 0, 0
		if captured != nil {
			result.Stats.BlobBytesDownloaded = captured.stats.BlobBytesDownloaded
			result.Stats.ManifestBytesDownloaded = captured.stats.ManifestBytesDownloaded
		}
		if retErr == nil {
			s.progress(Progress{Operation: "pulled", Repository: reference, Descriptor: root.Descriptor, Stats: result.Stats})
		}
	}()
	if err := copyGraph(ctx, source, target, root.Descriptor, &result.Stats, nil); err != nil {
		return result, err
	}
	if _, err := bundle.LoadSnapshotGraph(ctx, target, root.Descriptor); err != nil {
		return result, operationError("verify", reference, root.Descriptor, ErrIntegrity, err)
	}
	return result, nil
}

// MaterializeInto copies a pinned local bundle into caller-owned storage,
// fetching only missing opaque payloads allowed by its validated capture state.
// Root.Store and target may be the same CAS. Tags and plugins are never revisited.
func (s *Service) MaterializeInto(ctx context.Context, root Root, state *CaptureState, target content.Storage) (result PullResult, retErr error) {
	result = PullResult{Descriptor: root.Descriptor, Mode: PullLocked, Stats: Stats{Accounting: statsAccounting}}
	defer func() {
		if retErr != nil {
			retErr = operationError("materialize", root.Repository, root.Descriptor, nil, retErr)
		}
	}()
	if root.Store == nil || target == nil {
		return result, operationError("materialize", root.Repository, root.Descriptor, ErrInvalid, fmt.Errorf("source and target storage are required"))
	}
	snapshot, err := bundle.InspectSnapshotGraph(ctx, root.Store, root.Descriptor)
	if err != nil {
		return result, operationError("verify", root.Repository, root.Descriptor, ErrIntegrity, err)
	}
	var source content.Fetcher = root.Store
	if state != nil {
		pinned := cloneJSON(*state)
		if err := ValidateCaptureState(ctx, root.Store, snapshot, pinned); err != nil {
			return result, operationError("capture", root.Repository, root.Descriptor, ErrIntegrity, err)
		}
		source = captureSource{local: root.Store, service: s, sources: pinned.Sources, stats: &result.Stats}
	}
	defer func() {
		if _, remote := registryBlobs(root.Store); remote && state == nil {
			result.Stats.BlobBytesDownloaded = result.Stats.BlobBytesUploaded
			result.Stats.ManifestBytesDownloaded = result.Stats.ManifestBytesUploaded
		}
		result.Stats.BlobBytesUploaded, result.Stats.ManifestBytesUploaded = 0, 0
	}()
	if err := copyGraph(ctx, source, target, root.Descriptor, &result.Stats, nil); err != nil {
		return result, err
	}
	if _, err := bundle.LoadSnapshotGraph(ctx, target, root.Descriptor); err != nil {
		return result, operationError("verify", root.Repository, root.Descriptor, ErrIntegrity, err)
	}
	return result, nil
}
