package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
)

// AttachRequest supplies a prepared OCI manifest or index with a subject. Store
// must contain its strong dependency closure and remains owned by the caller.
type AttachRequest struct {
	Subject    string
	Descriptor v1.Descriptor
	Store      content.ReadOnlyStorage
}

// AttachResult distinguishes complete publication from confirmed discovery.
// Both flags retain confirmed work when Attach fails. Stats counts completed SDK
// copy operations; interrupted inventory updates can leave accepted manifest
// writes outside that accounting.
type AttachResult struct {
	Repository   string        `json:"repository"`
	Subject      v1.Descriptor `json:"subject"`
	Descriptor   v1.Descriptor `json:"descriptor"`
	Stats        Stats         `json:"stats"`
	Published    bool          `json:"published"`
	Discoverable bool          `json:"discoverable"`
}

type ListReferrersOptions struct {
	ArtifactType string
}

func (s *Service) Attach(ctx context.Context, request AttachRequest) (AttachResult, error) {
	request.Descriptor = cloneDescriptor(request.Descriptor)
	result := AttachResult{Descriptor: request.Descriptor, Stats: Stats{Accounting: statsAccounting}}
	fail := func(kind, err error) (AttachResult, error) {
		return result, operationError("attach", result.Repository, request.Descriptor, kind, err)
	}
	if err := ctx.Err(); err != nil {
		return fail(nil, err)
	}
	if request.Store == nil {
		return fail(ErrInvalid, fmt.Errorf("attachment needs a source store"))
	}
	if request.Descriptor.MediaType != v1.MediaTypeImageManifest && request.Descriptor.MediaType != v1.MediaTypeImageIndex {
		return fail(ErrUnsupported, fmt.Errorf("attachment must be an OCI manifest or index"))
	}
	if err := request.Descriptor.Digest.Validate(); err != nil {
		return fail(ErrInvalid, fmt.Errorf("attachment descriptor digest: %w", err))
	}
	if request.Descriptor.Size < 0 || !request.Descriptor.Digest.Algorithm().Available() {
		return fail(ErrInvalid, fmt.Errorf("attachment descriptor needs a nonnegative size and supported digest algorithm"))
	}
	if err := s.acquirePublication(ctx); err != nil {
		return fail(nil, err)
	}
	defer s.releasePublication()
	subject, err := s.Resolve(ctx, request.Subject)
	if err != nil {
		return fail(nil, err)
	}
	result.Repository = subject.Repository
	result.Subject = cloneDescriptor(subject.Descriptor)
	return s.attachPinned(ctx, request, result.Repository, result.Subject)
}

// attachPinned runs under the caller's publication permit against an immutable
// subject. Bundle delivery uses it without recursively acquiring that permit.
func (s *Service) attachPinned(ctx context.Context, request AttachRequest, repository string, subject v1.Descriptor) (AttachResult, error) {
	result := AttachResult{Repository: repository, Subject: cloneDescriptor(subject), Descriptor: cloneDescriptor(request.Descriptor), Stats: Stats{Accounting: statsAccounting}}
	fail := func(kind, err error) (AttachResult, error) {
		return result, operationError("attach", repository, request.Descriptor, kind, err)
	}
	raw, err := fetchEnvelopeBytes(ctx, request.Store, request.Descriptor)
	if err != nil {
		return fail(nil, err)
	}
	env, err := bundle.ParseEnvelope(request.Descriptor, raw)
	if err != nil {
		return fail(ErrIntegrity, err)
	}
	if env.Subject == nil || !content.Equal(*env.Subject, result.Subject) {
		return fail(ErrInvalid, fmt.Errorf("attachment subject must match the resolved subject digest, size, and media type"))
	}
	// Validate the authoritative source before the first registry write, including
	// children that the destination might already contain.
	if err := bundle.VerifyClosure(ctx, attachmentSource{request.Store}, request.Descriptor); err != nil {
		kind := ErrIntegrity
		var sourceErr *attachmentSourceError
		if errors.As(err, &sourceErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			kind = nil
		}
		return fail(kind, err)
	}
	targets := []DeliveryRepository{{Repository: result.Repository, Roots: []v1.Descriptor{request.Descriptor}}}
	if err := s.preflightManagedTags(ctx, request.Store, targets, nil); err != nil {
		return fail(nil, err)
	}
	repo, err := s.repository(result.Repository)
	if err != nil {
		return fail(ErrInvalid, err)
	}
	if err := copyGraph(ctx, request.Store, repo, request.Descriptor, &result.Stats, nil); err != nil {
		// A manifest PUT can succeed before its fallback inventory update fails.
		// Confirm any completed graph without masking the original failure or
		// extending the caller's cancellation deadline. Failed ORAS operations
		// retain conservative accounting because their writes are not reported.
		if verifyRemoteClosure(ctx, repo, request.Descriptor) == nil {
			result.Published = true
		}
		return fail(nil, err)
	}
	if err := verifyRemoteClosure(ctx, repo, request.Descriptor); err != nil {
		return fail(nil, err)
	}
	result.Published = true
	s.progress(Progress{Operation: "attachment-published", Repository: result.Repository, Descriptor: request.Descriptor, Stats: result.Stats})
	refs, err := listPinnedReferrers(ctx, repo, result.Subject, ListReferrersOptions{})
	if err != nil {
		return fail(nil, err)
	}
	if !hasAttachment(refs, request.Descriptor) {
		// Re-publishing identical bytes repairs an interrupted fallback inventory
		// update, even when the manifest was already present before this call.
		err := repo.Push(ctx, request.Descriptor, bytes.NewReader(raw))
		if err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			return fail(nil, err)
		}
		if err == nil {
			result.Stats.ManifestBytesUploaded += int64(len(raw))
		} else {
			result.Stats.ObjectsSkipped++
		}
		refs, err = listPinnedReferrers(ctx, repo, result.Subject, ListReferrersOptions{})
		if err != nil {
			return fail(nil, err)
		}
		if !hasAttachment(refs, request.Descriptor) {
			return fail(ErrIntegrity, fmt.Errorf("published attachment is not discoverable from subject %s", result.Subject.Digest))
		}
	}
	result.Discoverable = true
	s.progress(Progress{Operation: "attached", Repository: result.Repository, Descriptor: request.Descriptor, Stats: result.Stats})
	return result, nil
}

// ListReferrers pins the subject once, then validates every returned descriptor
// against its actual remote envelope. It returns no unverified inventory hints.
func (s *Service) ListReferrers(ctx context.Context, subjectReference string, options ListReferrersOptions) ([]v1.Descriptor, error) {
	root, err := s.Resolve(ctx, subjectReference)
	if err != nil {
		return nil, operationError("list referrers", subjectReference, v1.Descriptor{}, nil, err)
	}
	repo, err := s.repository(root.Repository)
	if err != nil {
		return nil, operationError("list referrers", root.Repository, root.Descriptor, ErrInvalid, err)
	}
	refs, err := listPinnedReferrers(ctx, repo, root.Descriptor, options)
	return refs, operationError("list referrers", root.Repository, root.Descriptor, nil, err)
}

func listPinnedReferrers(ctx context.Context, repo *remote.Repository, subject v1.Descriptor, options ListReferrersOptions) ([]v1.Descriptor, error) {
	// Keep the repository's learned or explicitly configured ORAS capability
	// state. Only inventory requests receive the validation marker; the actual
	// referrer Resolve/Fetch operations below retain the original context.
	inventoryCtx := context.WithValue(ctx, inventoryValidationKey{}, inventoryValidationOptions{subject: subject, maxBytes: repo.MaxMetadataBytes})
	result := []v1.Descriptor{}
	seen := map[string]v1.Descriptor{}
	err := repo.Referrers(inventoryCtx, subject, "", func(refs []v1.Descriptor) error {
		for _, advertised := range refs {
			// Capture needs to classify each candidate's actual artifact type.
			// Bound that read before buffering an advertised envelope, including
			// candidates whose inventory omits or misstates their artifact type.
			limit, _ := ctx.Value(dependencyMetadataLimitKey{}).(int64)
			if limit > 0 && advertised.Size > limit {
				return fmt.Errorf("%w: referrer metadata exceeds dependency discovery limit of %d bytes", ErrUnsupported, limit)
			}
			if err := advertised.Digest.Validate(); err != nil {
				return fmt.Errorf("%w: invalid referrer digest: %w", ErrIntegrity, err)
			}
			if advertised.Size < 0 || !advertised.Digest.Algorithm().Available() {
				return fmt.Errorf("%w: invalid referrer size or digest algorithm", ErrIntegrity)
			}
			if advertised.MediaType != v1.MediaTypeImageManifest && advertised.MediaType != v1.MediaTypeImageIndex {
				return fmt.Errorf("%w: referrer is not an OCI manifest or index", ErrIntegrity)
			}
			actual, err := repo.Resolve(ctx, advertised.Digest.String())
			if err != nil {
				return err
			}
			if limit > 0 && actual.Size > limit {
				return fmt.Errorf("%w: referrer metadata exceeds dependency discovery limit of %d bytes", ErrUnsupported, limit)
			}
			if !content.Equal(actual, advertised) {
				return fmt.Errorf("%w: referrer descriptor does not match remote envelope", ErrIntegrity)
			}
			// Fetch from the manifest endpoint, even when inventory embeds Data.
			if budget, _ := ctx.Value(destinationInspectionBudgetKey{}).(*destinationInspectionBudget); budget != nil {
				if err := budget.charge(actual.Size); err != nil {
					return err
				}
			}
			raw, err := content.FetchAll(ctx, repo, actual)
			if err != nil {
				return err
			}
			env, err := bundle.ParseEnvelope(advertised, raw)
			if err != nil {
				return fmt.Errorf("%w: %w", ErrIntegrity, err)
			}
			if env.Subject == nil || !content.Equal(*env.Subject, subject) {
				return fmt.Errorf("%w: listed referrer does not reference the pinned subject", ErrIntegrity)
			}
			validated := cloneDescriptor(advertised)
			validated.ArtifactType = env.ArtifactType
			key := validated.Digest.String()
			if prior, ok := seen[key]; ok {
				if !content.Equal(prior, validated) || prior.ArtifactType != validated.ArtifactType {
					return fmt.Errorf("%w: conflicting referrer descriptors", ErrIntegrity)
				}
				continue
			}
			seen[key] = validated
			if options.ArtifactType == "" || env.ArtifactType == options.ArtifactType {
				result = append(result, validated)
			}
		}
		return nil
	})
	sort.Slice(result, func(i, j int) bool { return result[i].Digest.String() < result[j].Digest.String() })
	return result, err
}

func hasAttachment(refs []v1.Descriptor, expected v1.Descriptor) bool {
	for _, ref := range refs {
		if content.Equal(ref, expected) {
			return true
		}
	}
	return false
}

// Source failures preserve transport/authentication errors rather than
// classifying every failed source read as malformed artifact content.
type attachmentSourceError struct{ error }

func (e *attachmentSourceError) Unwrap() error { return e.error }

type attachmentSource struct{ content.ReadOnlyStorage }

func (s attachmentSource) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	r, err := s.ReadOnlyStorage.Fetch(ctx, d)
	if repo, ok := s.ReadOnlyStorage.(*remote.Repository); ok && errors.Is(err, errdef.ErrNotFound) && bundle.IsEnvelope(d.MediaType) {
		r, err = repo.Blobs().Fetch(ctx, d)
	}
	if err != nil {
		return nil, &attachmentSourceError{err}
	}
	return attachmentReader{r}, nil
}

type attachmentReader struct{ io.ReadCloser }

func (r attachmentReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = &attachmentSourceError{err}
	}
	return n, err
}
func (r attachmentReader) Close() error {
	if err := r.ReadCloser.Close(); err != nil {
		return &attachmentSourceError{err}
	}
	return nil
}

// validatedRegistryReferrers is installed only for repositories created by
// Service, whose HTTP client supports request-scoped inventory validation.
type validatedRegistryReferrers struct{ repo *remote.Repository }

func (r validatedRegistryReferrers) Referrers(ctx context.Context, subject v1.Descriptor, artifactType string, fn func([]v1.Descriptor) error) error {
	refs, err := listPinnedReferrers(ctx, r.repo, subject, ListReferrersOptions{ArtifactType: artifactType})
	if len(refs) != 0 || err == nil {
		if callbackErr := fn(refs); callbackErr != nil {
			return callbackErr
		}
	}
	return err
}
