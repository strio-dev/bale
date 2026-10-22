package transport

import (
	"context"
	"errors"
	"fmt"
	"strings"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/dependency"
)

// InspectDependencies discovers declarations of one pinned registry subject.
// It reuses capture's metadata limits and declaration validation without
// selecting a policy, expanding references, or verifying application payloads.
func (s *Service) InspectDependencies(ctx context.Context, subjectReference string) ([]InspectedDeclaration, error) {
	root, err := s.Resolve(ctx, subjectReference)
	if err != nil {
		return nil, operationError("inspect dependencies", subjectReference, v1.Descriptor{}, nil, err)
	}
	fail := func(kind, err error) ([]InspectedDeclaration, error) {
		return nil, operationError("inspect dependencies", root.Repository, root.Descriptor, kind, err)
	}
	repo, err := s.repository(root.Repository)
	if err != nil {
		return fail(ErrInvalid, err)
	}
	metadataCtx := context.WithValue(ctx, dependencyMetadataLimitKey{}, dependency.MaxBytes)
	refs, err := listPinnedReferrers(metadataCtx, repo, root.Descriptor, ListReferrersOptions{})
	if err != nil {
		return fail(nil, err)
	}
	result := []InspectedDeclaration{}
	for _, desc := range refs {
		if !strings.HasPrefix(desc.ArtifactType, "application/vnd.bale.dependencies.") {
			continue
		}
		if desc.ArtifactType != dependency.ArtifactType {
			return fail(ErrUnsupported, fmt.Errorf("unsupported dependency declaration type %s", desc.ArtifactType))
		}
		declaration, err := bundle.LoadDependencyDeclaration(ctx, attachmentSource{repo}, desc, root.Descriptor)
		if err != nil {
			kind := ErrIntegrity
			var sourceErr *attachmentSourceError
			if errors.As(err, &sourceErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				kind = nil
			}
			return fail(kind, operationError("dependency declaration", root.Repository, desc, nil, err))
		}
		result = append(result, InspectedDeclaration{Repository: root.Repository, Subject: cloneDescriptor(root.Descriptor), Descriptor: desc, Declaration: declaration})
	}
	return result, nil
}
