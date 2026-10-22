package transport

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

type managedTag struct {
	repository string
	reference  string
	subject    v1.Descriptor
}

// managedReferrerTags follows the same envelope edges as copyGraph. Subject
// backlinks do not add roots; each copied OCI subject-bearing envelope reserves
// its standard fallback tag in the repository receiving those bytes.
func managedReferrerTags(ctx context.Context, src content.Fetcher, targets []DeliveryRepository) ([]managedTag, error) {
	managed := map[string]managedTag{}
	seen := map[string]bool{}
	var walk func(string, v1.Descriptor, bool) error
	walk = func(repository string, d v1.Descriptor, isEnvelope bool) error {
		if !isEnvelope {
			return nil
		}
		key := repository + "@" + visitKey(d, true)
		if seen[key] {
			return nil
		}
		seen[key] = true
		raw, err := fetchEnvelopeBytes(ctx, src, d)
		if err != nil {
			return err
		}
		env, err := bundle.ParseEnvelope(d, raw)
		if err != nil {
			return err
		}
		if env.Subject != nil && (d.MediaType == v1.MediaTypeImageManifest || d.MediaType == v1.MediaTypeImageIndex) {
			name := strings.Replace(env.Subject.Digest.String(), ":", "-", 1)
			coordinate := repository + ":" + name
			if prior, ok := managed[coordinate]; ok && prior.subject.Size != env.Subject.Size {
				return fmt.Errorf("%w: conflicting subject size for managed referrer tag %s", ErrIntegrity, coordinate)
			}
			managed[coordinate] = managedTag{repository: repository, reference: name, subject: *env.Subject}
		}
		for i, child := range env.Children {
			if err := walk(repository, child, bundle.IsEnvelopeChild(d, i, child)); err != nil {
				return err
			}
		}
		return nil
	}
	for _, target := range targets {
		for _, root := range target.Roots {
			if err := walk(target.Repository, root, true); err != nil {
				return nil, err
			}
		}
	}
	result := make([]managedTag, 0, len(managed))
	for _, tag := range managed {
		result = append(result, tag)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].repository+":"+result[i].reference < result[j].repository+":"+result[j].reference
	})
	return result, nil
}

func checkManagedTagCollisions(managed []managedTag, published []Tag) error {
	reserved := map[string]bool{}
	for _, tag := range managed {
		reserved[tag.repository+":"+tag.reference] = true
	}
	for _, tag := range published {
		if reserved[tag.Repository+":"+tag.Reference] {
			return fmt.Errorf("%w: publication collides with managed referrer tag %s:%s", ErrConflict, tag.Repository, tag.Reference)
		}
	}
	return nil
}

func (s *Service) preflightManagedTags(ctx context.Context, src content.Fetcher, targets []DeliveryRepository, published []Tag) error {
	managed, err := managedReferrerTags(ctx, src, targets)
	if err != nil {
		return err
	}
	if err := checkManagedTagCollisions(managed, published); err != nil {
		return err
	}
	for _, tag := range managed {
		if err := s.validateManagedInventory(ctx, tag); err != nil {
			return fmt.Errorf("managed referrer tag %s:%s: %w", tag.repository, tag.reference, err)
		}
	}
	return nil
}

func (s *Service) validateManagedInventory(ctx context.Context, tag managedTag) error {
	repo, err := s.repository(tag.repository)
	if err != nil {
		return err
	}
	d, err := repo.Resolve(ctx, tag.reference)
	if errors.Is(err, errdef.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if d.Digest == tag.subject.Digest {
		return fmt.Errorf("%w: existing tag points to the subject itself, not its referrers inventory", ErrConflict)
	}
	if d.MediaType != v1.MediaTypeImageIndex {
		return fmt.Errorf("%w: existing content is not an OCI referrers inventory", ErrConflict)
	}
	raw, err := content.FetchAll(ctx, repo, d)
	if err != nil {
		return err
	}
	env, err := bundle.ParseEnvelope(d, raw)
	if err != nil {
		return fmt.Errorf("%w: invalid referrers inventory: %w", ErrConflict, err)
	}
	if env.Subject != nil {
		return fmt.Errorf("%w: existing inventory unexpectedly has a subject", ErrConflict)
	}
	for _, child := range env.Children {
		if child.MediaType != v1.MediaTypeImageManifest && child.MediaType != v1.MediaTypeImageIndex {
			return fmt.Errorf("%w: inventory child %s is not an OCI envelope", ErrConflict, child.Digest)
		}
		raw, err := content.FetchAll(ctx, repo, child)
		if err != nil {
			return fmt.Errorf("inventory child %s: %w", child.Digest, err)
		}
		referrer, err := bundle.ParseEnvelope(child, raw)
		if err != nil {
			return fmt.Errorf("%w: invalid inventory child: %w", ErrConflict, err)
		}
		if referrer.Subject == nil || referrer.Subject.Digest != tag.subject.Digest || referrer.Subject.Size != tag.subject.Size {
			return fmt.Errorf("%w: inventory child %s does not reference subject %s", ErrConflict, child.Digest, tag.subject.Digest)
		}
	}
	return nil
}
