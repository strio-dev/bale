package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
)

// InspectResult contains verified captured metadata, without refreshing selectors
// or discovering additional referrers. Declarations belong to the root bundle;
// nested native bundles can be inspected separately using their pinned roots.
type InspectResult struct {
	MetadataOnly bool                   `json:"metadataOnly,omitempty"`
	Descriptor   v1.Descriptor          `json:"descriptor"`
	Metadata     bundle.Metadata        `json:"metadata"`
	Declarations []InspectedDeclaration `json:"declarations"`
}

// InspectedDeclaration pairs a verified declaration with its pinned subject and
// repository. InspectDependencies reads live attachments; bundle inspection
// returns declarations selected in that bundle's captured inventory.
type InspectedDeclaration struct {
	Repository  string                 `json:"repository"`
	Subject     v1.Descriptor          `json:"subject"`
	Descriptor  v1.Descriptor          `json:"descriptor"`
	Declaration dependency.Declaration `json:"declaration"`
}

// Inspect pins a remote reference once and verifies only its captured graph.
func (s *Service) Inspect(ctx context.Context, reference string) (InspectResult, error) {
	root, err := s.Resolve(ctx, reference)
	if err != nil {
		return InspectResult{}, err
	}
	return s.InspectRoot(ctx, root)
}

// InspectLayout reads a completed single-root OCI bundle layout without writes.
func (s *Service) InspectLayout(ctx context.Context, path string) (InspectResult, error) {
	return s.inspectLayout(ctx, path, bundle.LoadSnapshotGraph)
}

func (s *Service) inspectLayout(ctx context.Context, path string, load func(context.Context, content.ReadOnlyStorage, v1.Descriptor) (bundle.Snapshot, error)) (InspectResult, error) {
	fail := func(kind, err error) (InspectResult, error) {
		return InspectResult{}, operationError("inspect layout", path, v1.Descriptor{}, kind, err)
	}
	if err := ctx.Err(); err != nil {
		return fail(nil, err)
	}
	if strings.TrimSpace(path) == "" {
		return fail(ErrInvalid, fmt.Errorf("layout path is required"))
	}
	read := func(name string, target any) error {
		raw, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, target)
	}
	var layout v1.ImageLayout
	if err := read("oci-layout", &layout); err != nil {
		return fail(layoutReadKind(err), err)
	}
	if layout.Version != v1.ImageLayoutVersion {
		return fail(ErrIntegrity, fmt.Errorf("invalid OCI layout version"))
	}
	var index v1.Index
	if err := read("index.json", &index); err != nil {
		return fail(layoutReadKind(err), err)
	}
	if index.SchemaVersion != 2 || len(index.Manifests) != 1 {
		return fail(ErrIntegrity, fmt.Errorf("layout must contain exactly one bundle root"))
	}
	store := oci.NewStorageFromFS(os.DirFS(path))
	_, markerErr := os.Lstat(filepath.Join(path, captureFile))
	metadataOnly := markerErr == nil
	if markerErr != nil && !os.IsNotExist(markerErr) {
		return fail(nil, markerErr)
	}
	if metadataOnly {
		load = bundle.InspectSnapshotGraph
	}
	result, err := s.inspectRoot(ctx, Root{Descriptor: index.Manifests[0], Store: store}, load)
	if err != nil {
		return InspectResult{}, err
	}
	if metadataOnly {
		snapshot := bundle.Snapshot{Descriptor: result.Descriptor, Metadata: result.Metadata}
		if _, _, err := readLocalCapture(ctx, path, store, snapshot); err != nil {
			return fail(ErrIntegrity, err)
		}
		result.MetadataOnly = true
	}
	return result, nil
}

func layoutReadKind(err error) error {
	if os.IsNotExist(err) {
		return ErrNotFound
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return nil
	}
	return ErrIntegrity
}

// InspectRoot verifies a pinned bundle in a caller-owned store. It does not
// discover referrers, resolve source selectors, publish content, or close Store.
func (s *Service) InspectRoot(ctx context.Context, root Root) (InspectResult, error) {
	return s.inspectRoot(ctx, root, bundle.LoadSnapshotGraph)
}

func (s *Service) inspectRoot(ctx context.Context, root Root, load func(context.Context, content.ReadOnlyStorage, v1.Descriptor) (bundle.Snapshot, error)) (InspectResult, error) {
	fail := func(kind, err error) (InspectResult, error) {
		return InspectResult{}, operationError("inspect", root.Repository, root.Descriptor, kind, err)
	}
	if err := ctx.Err(); err != nil {
		return fail(nil, err)
	}
	if root.Store == nil {
		return fail(ErrInvalid, fmt.Errorf("inspection needs a source store"))
	}
	if err := root.Descriptor.Digest.Validate(); err != nil {
		return fail(ErrInvalid, err)
	}
	if root.Descriptor.Size < 0 || !root.Descriptor.Digest.Algorithm().Available() || root.Descriptor.MediaType == "" {
		return fail(ErrInvalid, fmt.Errorf("invalid root descriptor"))
	}
	if !bundle.IsEnvelope(root.Descriptor.MediaType) {
		return fail(ErrUnsupported, fmt.Errorf("inspection requires a native Bale bundle"))
	}
	store := attachmentSource{root.Store}
	validationFailure := func(err error) (InspectResult, error) {
		kind := ErrIntegrity
		var sourceErr *attachmentSourceError
		if errors.As(err, &sourceErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			kind = nil
		}
		return fail(kind, err)
	}
	raw, err := fetchEnvelopeBytes(ctx, store, root.Descriptor)
	if err != nil {
		return validationFailure(err)
	}
	envelope, err := bundle.ParseEnvelope(root.Descriptor, raw)
	if err != nil {
		return validationFailure(err)
	}
	if root.Descriptor.MediaType != v1.MediaTypeImageIndex || envelope.ArtifactType != bundle.BundleArtifactType {
		return fail(ErrUnsupported, fmt.Errorf("inspection requires a native Bale bundle"))
	}
	snapshot, err := load(ctx, store, root.Descriptor)
	if err != nil {
		return validationFailure(err)
	}
	result := InspectResult{Descriptor: snapshot.Descriptor, Metadata: snapshot.Metadata, Declarations: []InspectedDeclaration{}}
	seen := map[string]bool{}
	for _, resolution := range snapshot.Metadata.Dependencies {
		if resolution.Outcome != "selected" || resolution.Declaration == nil {
			continue
		}
		key := resolution.Repository + "@" + resolution.Subject.Digest.String() + "@" + resolution.Declaration.Digest.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		declaration, err := bundle.LoadDependencyDeclaration(ctx, store, *resolution.Declaration, resolution.Subject)
		if err != nil {
			return validationFailure(err)
		}
		result.Declarations = append(result.Declarations, InspectedDeclaration{Repository: resolution.Repository, Subject: resolution.Subject, Descriptor: *resolution.Declaration, Declaration: declaration})
	}
	sort.Slice(result.Declarations, func(i, j int) bool {
		a, b := result.Declarations[i], result.Declarations[j]
		return a.Repository+"@"+a.Subject.Digest.String()+"@"+a.Descriptor.Digest.String() < b.Repository+"@"+b.Subject.Digest.String()+"@"+b.Descriptor.Digest.String()
	})
	return result, nil
}
