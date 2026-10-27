package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/destination"
	"oras.land/oras-go/v2/content"
)

// InspectedDestination pairs a verified record with its exact registry subject.
// Local means the record's destination names this repository and digest. It
// makes no claim about recency, bundle completion or current target availability.
type InspectedDestination struct {
	Repository string             `json:"repository"`
	Subject    v1.Descriptor      `json:"subject"`
	Descriptor v1.Descriptor      `json:"descriptor"`
	Record     destination.Record `json:"record"`
	Local      bool               `json:"local"`
}

// DestinationInspectionOptions bounds a single read-only inspection. Zero limits
// preserve existing behavior. Metadata bytes include inventory responses and
// attachment manifest/config/payload reads, including validation rereads.
type DestinationInspectionOptions struct {
	MaxMetadataBytes int64
	MaxReferrers     int
}

type destinationInspectionBudgetKey struct{}
type destinationInspectionBudget struct {
	remaining               int64
	referrers, maxReferrers int
}

func (b *destinationInspectionBudget) charge(size int64) error {
	if size < 0 || size > b.remaining {
		return fmt.Errorf("%w: destination inspection metadata byte limit exceeded", ErrUnsupported)
	}
	b.remaining -= size
	return nil
}

type budgetedAttachmentSource struct {
	content.ReadOnlyStorage
	budget *destinationInspectionBudget
}

func (s budgetedAttachmentSource) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	if err := s.budget.charge(d.Size); err != nil {
		return nil, err
	}
	return s.ReadOnlyStorage.Fetch(ctx, d)
}

// InspectDestinations reads current and carried historical destination records.
// It resolves only the subject, validates metadata and returns all records in
// digest order. Errors return no partial results; no targets are contacted.
func (s *Service) InspectDestinations(ctx context.Context, subjectReference string) ([]InspectedDestination, error) {
	return s.InspectDestinationsWithOptions(ctx, subjectReference, DestinationInspectionOptions{})
}

// InspectDestinationsWithOptions applies explicit aggregate metadata and inventory
// limits before retaining descriptors or fetching additional attachment content.
// Errors return no partial results. The subject is resolved once.
func (s *Service) InspectDestinationsWithOptions(ctx context.Context, subjectReference string, options DestinationInspectionOptions) ([]InspectedDestination, error) {
	if options.MaxMetadataBytes < 0 || options.MaxReferrers < 0 {
		return nil, operationError("inspect destinations", subjectReference, v1.Descriptor{}, ErrInvalid, fmt.Errorf("inspection limits must be nonnegative"))
	}
	var budget *destinationInspectionBudget
	if options.MaxMetadataBytes > 0 || options.MaxReferrers > 0 {
		remaining := options.MaxMetadataBytes
		if remaining == 0 {
			remaining = 1<<63 - 1
		}
		budget = &destinationInspectionBudget{remaining: remaining, maxReferrers: options.MaxReferrers}
		ctx = context.WithValue(ctx, destinationInspectionBudgetKey{}, budget)
	}
	root, err := s.Resolve(ctx, subjectReference)
	if err != nil {
		return nil, operationError("inspect destinations", subjectReference, v1.Descriptor{}, nil, err)
	}
	fail := func(kind, err error) ([]InspectedDestination, error) {
		return nil, operationError("inspect destinations", root.Repository, root.Descriptor, kind, err)
	}
	repo, err := s.repository(ctx, root.Repository)
	if err != nil {
		return fail(nil, err)
	}
	metadataCtx := context.WithValue(ctx, dependencyMetadataLimitKey{}, destination.MaxBytes)
	refs, err := listPinnedReferrers(metadataCtx, repo, root.Descriptor, ListReferrersOptions{})
	if err != nil {
		return fail(nil, err)
	}
	result := []InspectedDestination{}
	for _, desc := range refs {
		if !strings.HasPrefix(desc.ArtifactType, "application/vnd.bale.destination.") {
			continue
		}
		if desc.ArtifactType != destination.ArtifactType {
			return fail(ErrUnsupported, fmt.Errorf("unsupported destination record type %s", desc.ArtifactType))
		}
		var source content.ReadOnlyStorage = attachmentSource{repo}
		if budget != nil {
			source = budgetedAttachmentSource{source, budget}
		}
		record, err := destination.Load(ctx, source, desc, root.Descriptor)
		if err != nil {
			kind := ErrIntegrity
			var sourceErr *attachmentSourceError
			if errors.Is(err, ErrUnsupported) || errors.As(err, &sourceErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				kind = nil
			}
			return fail(kind, operationError("destination record", root.Repository, desc, nil, err))
		}
		local := record.Destination == root.Repository+"@"+root.Descriptor.Digest.String()
		result = append(result, InspectedDestination{Repository: root.Repository, Subject: cloneDescriptor(root.Descriptor), Descriptor: desc, Record: record, Local: local})
	}
	return result, nil
}
