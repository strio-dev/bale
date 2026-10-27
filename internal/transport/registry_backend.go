package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
)

// registryRepository preserves the default ORAS path while owning validation and
// fallback indexing for caller-owned content operations.
type registryRepository struct {
	Reference        registry.Reference
	MaxMetadataBytes int64
	remote           *remote.Repository
	backend          RegistryRepository
}

type registryTarget interface {
	content.Storage
	content.TagResolver
}

func (r *registryRepository) target() registryTarget {
	if r.remote != nil {
		return r.remote
	}
	return r.backend
}

func (r *registryRepository) Blobs() RegistryBlobStore {
	if r.remote != nil {
		return r.remote.Blobs()
	}
	return r.backend.Blobs()
}

func (r *registryRepository) Resolve(ctx context.Context, reference string) (v1.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	return r.target().Resolve(ctx, reference)
}

func (r *registryRepository) Fetch(ctx context.Context, expected v1.Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !bundle.IsEnvelope(expected.MediaType) {
		return r.Blobs().Fetch(ctx, expected)
	}
	return r.target().Fetch(ctx, expected)
}

func (r *registryRepository) Exists(ctx context.Context, expected v1.Descriptor) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !bundle.IsEnvelope(expected.MediaType) {
		return r.Blobs().Exists(ctx, expected)
	}
	return r.target().Exists(ctx, expected)
}

func (r *registryRepository) Tag(ctx context.Context, expected v1.Descriptor, reference string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.target().Tag(ctx, expected, reference)
}

func (r *registryRepository) Tags(ctx context.Context, last string, fn func([]string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.remote != nil {
		return r.remote.Tags(ctx, last, fn)
	}
	if lister, ok := r.backend.(registry.TagLister); ok {
		return lister.Tags(ctx, last, fn)
	}
	return fmt.Errorf("%w: caller-owned repository cannot list tags", ErrUnsupported)
}

func (r *registryRepository) Push(ctx context.Context, expected v1.Descriptor, reader io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.remote != nil {
		return r.remote.Push(ctx, expected, reader)
	}
	raw, err := content.ReadAll(reader, expected)
	if err != nil {
		return err
	}
	envelope, err := bundle.ParseEnvelope(expected, raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrIntegrity, err)
	}
	pushErr := r.backend.Push(ctx, expected, bytes.NewReader(raw))
	if pushErr != nil && !errors.Is(pushErr, errdef.ErrAlreadyExists) {
		return pushErr
	}
	if envelope.Subject == nil {
		return pushErr
	}
	// Native success does not imply that a specific referrer is visible. The
	// caller verifies discoverability separately and may republish once to repair.
	_, native, err := r.nativeReferrers(ctx, *envelope.Subject)
	if err != nil {
		return err
	}
	if !native {
		expected.ArtifactType = envelope.ArtifactType
		if err := r.indexReferrer(ctx, *envelope.Subject, expected); err != nil {
			return err
		}
	}
	return pushErr
}

func (r *registryRepository) Referrers(ctx context.Context, subject v1.Descriptor, artifactType string, fn func([]v1.Descriptor) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.remote != nil {
		return r.remote.Referrers(ctx, subject, artifactType, fn)
	}
	refs, native, err := r.nativeReferrers(ctx, subject)
	if err != nil {
		return err
	}
	if !native {
		index, err := r.fallbackInventory(ctx, subject)
		if err != nil {
			return err
		}
		refs = index.Manifests
	}
	// listPinnedReferrers validates real envelope types before applying filters.
	// Keep this provider's behavior usable independently without trusting hints.
	if artifactType != "" {
		filtered := make([]v1.Descriptor, 0, len(refs))
		for _, ref := range refs {
			if ref.ArtifactType == artifactType {
				filtered = append(filtered, ref)
			}
		}
		refs = filtered
	}
	return fn(refs)
}

// nativeReferrers distinguishes an explicitly fallback-only backend from a
// failed native query. A pagination error after any page must never fall back.
func (r *registryRepository) nativeReferrers(ctx context.Context, subject v1.Descriptor) ([]v1.Descriptor, bool, error) {
	refs := []v1.Descriptor{}
	pages := 0
	var callbackErr error
	budget, _ := ctx.Value(destinationInspectionBudgetKey{}).(*destinationInspectionBudget)
	err := r.backend.ReferrerPages(ctx, subject, func(page ReferrerPage) (err error) {
		if callbackErr != nil {
			return callbackErr
		}
		defer func() {
			if err != nil {
				callbackErr = err
			}
		}()
		pages++
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := validateInventoryPage(page, subject, r.MaxMetadataBytes, budget, false)
		if err != nil {
			return err
		}
		envelope, err := bundle.ParseEnvelope(content.NewDescriptorFromBytes(v1.MediaTypeImageIndex, raw), raw)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrIntegrity, err)
		}
		refs = append(refs, envelope.Children...)
		return nil
	})
	if callbackErr != nil {
		return nil, true, errors.Join(callbackErr, err)
	}
	if errors.Is(err, errdef.ErrUnsupported) && pages == 0 {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	return refs, true, nil
}
