package transport

import (
	"context"
	"fmt"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/config"
)

func (s *Service) Create(ctx context.Context, recipe config.Recipe, storageRef string, opts CreateOptions) (result CreateResult, retErr error) {
	defer func() {
		if retErr != nil {
			retErr = operationError("create", storageRef, result.Descriptor, nil, retErr)
		}
	}()

	target, err := completionRef(storageRef)
	if err != nil {
		return result, operationError("destination", storageRef, v1.Descriptor{}, ErrInvalid, err)
	}
	g, snapshot, err := s.capture(ctx, recipe)
	if err != nil {
		return result, err
	}
	result = CreateResult{Descriptor: snapshot.Descriptor, Metadata: snapshot.Metadata, DryRun: opts.DryRun, Stats: g.stats}
	defer func() { result.Stats.BlobBytesDownloaded = g.stats.BlobBytesDownloaded }()
	if opts.ExportDelivery {
		file, err := deliveryFromCapture(ctx, g, snapshot)
		if err != nil {
			return result, err
		}
		result.Delivery = &file
	}
	completion := Tag{Repository: repositoryName(target), Reference: target.Reference, Descriptor: snapshot.Descriptor}
	if !opts.DryRun {
		if err := s.acquirePublication(ctx); err != nil {
			return result, err
		}
		defer s.releasePublication()
	}
	if err := s.preflightManagedTags(ctx, g, []DeliveryRepository{{Repository: completion.Repository, Roots: []v1.Descriptor{snapshot.Descriptor}}}, []Tag{completion}); err != nil {
		return result, err
	}
	if err := s.checkTag(ctx, completion); err != nil {
		return result, err
	}
	if opts.DryRun {
		return result, nil
	}
	repo, err := s.repository(completion.Repository)
	if err != nil {
		return result, err
	}
	if err := copyGraph(ctx, g, repo, snapshot.Descriptor, &result.Stats, g.source); err != nil {
		return result, err
	}
	if err := verifyRemoteClosure(ctx, repo, snapshot.Descriptor); err != nil {
		return result, operationError("verify", completion.Repository, snapshot.Descriptor, nil, fmt.Errorf("verify stored capture: %w", err))
	}
	s.progress(Progress{Operation: "copied", Repository: completion.Repository, Descriptor: snapshot.Descriptor, Stats: result.Stats})
	if err := s.checkTag(ctx, completion); err != nil {
		return result, err
	}
	if err := repo.Tag(ctx, snapshot.Descriptor, target.Reference); err != nil {
		return result, err
	}
	d, err := repo.Resolve(ctx, target.Reference)
	if err != nil {
		return result, err
	}
	if d.Digest != snapshot.Descriptor.Digest {
		return result, operationError("tag", completion.Repository, completion.Descriptor, ErrConflict, fmt.Errorf("completion tag changed concurrently"))
	}
	result.Published = true
	s.progress(Progress{Operation: "tagged", Repository: completion.Repository, Descriptor: snapshot.Descriptor, Stats: result.Stats})
	return result, nil
}
