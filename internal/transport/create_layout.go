package transport

import (
	"context"

	"github.com/strio-dev/bale/internal/config"
)

// CreateLayout captures the same immutable graph as Create directly into a new
// OCI layout, without requiring or publishing to a holding registry.
func (s *Service) CreateLayout(ctx context.Context, recipe config.Recipe, layoutPath string, opts CreateOptions) (result CreateResult, retErr error) {
	defer func() {
		if retErr != nil {
			retErr = operationError("create", layoutPath, result.Descriptor, nil, retErr)
		}
	}()
	if err := checkLayoutDestination(layoutPath); err != nil {
		return result, err
	}
	g, snapshot, err := s.capture(ctx, recipe)
	if err != nil {
		return result, err
	}
	result = CreateResult{Descriptor: snapshot.Descriptor, Metadata: snapshot.Metadata, DryRun: opts.DryRun, Stats: g.stats}
	defer func() {
		result.Stats.BlobBytesDownloaded = g.stats.BlobBytesDownloaded
		result.Stats.ManifestBytesDownloaded = g.stats.ManifestBytesDownloaded
	}()
	if opts.ExportDelivery {
		file, err := deliveryFromCapture(ctx, g, snapshot)
		if err != nil {
			return result, err
		}
		result.Delivery = &file
	}
	if opts.DryRun {
		return result, nil
	}
	if err := writeLayout(ctx, g, snapshot.Descriptor, layoutPath, &Stats{}); err != nil {
		return result, err
	}
	result.LayoutPath = layoutPath
	s.progress(Progress{Operation: "created", Repository: layoutPath, Descriptor: result.Descriptor, Stats: result.Stats})
	return result, nil
}
