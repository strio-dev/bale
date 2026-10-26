package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

func referrersTag(subject v1.Descriptor) string {
	return strings.Replace(subject.Digest.String(), ":", "-", 1)
}

func (r *registryRepository) fallbackInventory(ctx context.Context, subject v1.Descriptor) (v1.Index, error) {
	empty := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{}}
	descriptor, err := r.Resolve(ctx, referrersTag(subject))
	if errors.Is(err, errdef.ErrNotFound) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	if descriptor.Digest == subject.Digest {
		return empty, fmt.Errorf("%w: fallback inventory resolves to the subject itself", ErrIntegrity)
	}
	reader, err := r.Fetch(ctx, descriptor)
	if err != nil {
		return empty, err
	}
	budget, _ := ctx.Value(destinationInspectionBudgetKey{}).(*destinationInspectionBudget)
	raw, validationErr := validateInventoryPage(ReferrerPage{MediaType: descriptor.MediaType, Digest: descriptor.Digest, Size: descriptor.Size, Content: reader}, subject, r.MaxMetadataBytes, budget, true)
	if err := errors.Join(validationErr, reader.Close()); err != nil {
		return empty, err
	}
	var index v1.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return empty, fmt.Errorf("%w: invalid referrers inventory: %w", ErrIntegrity, err)
	}
	return index, nil
}

// indexReferrer performs the standard fallback read/merge/write under Service's
// publication permit. Old inventory manifests remain available for readers.
// As with ORAS, callers coordinate overlapping writers in other clients/processes.
func (r *registryRepository) indexReferrer(ctx context.Context, subject, added v1.Descriptor) error {
	index, err := r.fallbackInventory(ctx, subject)
	if err != nil {
		return err
	}
	refs := make(map[string]v1.Descriptor, len(index.Manifests)+1)
	for _, advertised := range append(index.Manifests, added) {
		if advertised.MediaType != v1.MediaTypeImageManifest && advertised.MediaType != v1.MediaTypeImageIndex {
			return fmt.Errorf("%w: inventory child is not an OCI envelope", ErrIntegrity)
		}
		actual, err := r.Resolve(ctx, advertised.Digest.String())
		if err != nil {
			return err
		}
		if !content.Equal(actual, advertised) {
			return fmt.Errorf("%w: referrer descriptor does not match remote envelope", ErrIntegrity)
		}
		raw, err := content.FetchAll(ctx, r, actual)
		if err != nil {
			return err
		}
		envelope, err := bundle.ParseEnvelope(actual, raw)
		if err != nil {
			return fmt.Errorf("%w: invalid inventory child: %w", ErrIntegrity, err)
		}
		if envelope.Subject == nil || !content.Equal(*envelope.Subject, subject) {
			return fmt.Errorf("%w: inventory child does not reference the pinned subject", ErrIntegrity)
		}
		validated := cloneDescriptor(advertised)
		validated.ArtifactType = envelope.ArtifactType
		key := validated.Digest.String()
		if prior, ok := refs[key]; ok {
			if !content.Equal(prior, validated) || prior.ArtifactType != validated.ArtifactType {
				return fmt.Errorf("%w: conflicting referrer descriptors", ErrIntegrity)
			}
			continue
		}
		refs[key] = validated
	}
	index.Manifests = make([]v1.Descriptor, 0, len(refs))
	for _, ref := range refs {
		index.Manifests = append(index.Manifests, ref)
	}
	sort.Slice(index.Manifests, func(i, j int) bool { return index.Manifests[i].Digest.String() < index.Manifests[j].Digest.String() })
	raw, err := json.Marshal(index)
	if err != nil {
		return err
	}
	if int64(len(raw)) > inventoryByteLimit(r.MaxMetadataBytes) {
		return fmt.Errorf("%w: invalid referrers inventory: metadata size limit exceeded", ErrIntegrity)
	}
	descriptor := content.NewDescriptorFromBytes(v1.MediaTypeImageIndex, raw)
	// A tag update can fail after the new index bytes were accepted. Retrying
	// repeats this deterministic merge without discarding any existing entries.
	if err := r.backend.Push(ctx, descriptor, bytes.NewReader(raw)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return err
	}
	if err := r.Tag(ctx, descriptor, referrersTag(subject)); err != nil {
		return err
	}
	actual, err := r.Resolve(ctx, referrersTag(subject))
	if err != nil {
		return err
	}
	if !content.Equal(actual, descriptor) {
		return fmt.Errorf("%w: referrers inventory changed during publication", ErrConflict)
	}
	verified, err := content.FetchAll(ctx, r, actual)
	if err != nil {
		return err
	}
	if !bytes.Equal(verified, raw) {
		return fmt.Errorf("%w: published referrers inventory differs from prepared bytes", ErrIntegrity)
	}
	return nil
}
