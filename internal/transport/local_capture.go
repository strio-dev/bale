package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/jsonutil"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
)

const captureFile = "bale-capture.json"
const maxCaptureBytes = 16 << 20

// localCapture is local source-location metadata, not part of the transferable
// bundle graph. Only omitted opaque payloads can have remote locators.
type localCapture struct {
	SchemaVersion int                      `json:"schemaVersion"`
	Bundle        digest.Digest            `json:"bundle"`
	Sources       map[digest.Digest]string `json:"sources"`
}

// CreateLocal locks a recipe into a source-dependent local metadata capture.
// It retains verified envelopes and interpreted/generated metadata, while
// ordinary application payloads remain at their pinned source repositories.
func (s *Service) CreateLocal(ctx context.Context, recipe config.Recipe, path string, opts CreateOptions) (result CreateResult, retErr error) {
	defer func() {
		if retErr != nil {
			retErr = operationError("create", path, result.Descriptor, nil, retErr)
		}
	}()
	if err := checkLayoutDestination(path); err != nil {
		return result, err
	}
	g, snapshot, err := s.capture(ctx, recipe)
	if err != nil {
		return result, err
	}
	result = CreateResult{Descriptor: snapshot.Descriptor, Metadata: snapshot.Metadata, MetadataOnly: true, DryRun: opts.DryRun, Stats: g.stats}
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
	err = publishLocalLayout(ctx, path, snapshot.Descriptor, func(store *oci.Storage, directory string) error {
		recording := captureRecorder{source: captureDeliveryStore{g}, target: store}
		if _, err := bundle.InspectSnapshotGraph(ctx, recording, snapshot.Descriptor); err != nil {
			return err
		}
		// Inline content can share a digest with a non-inline edge. Retain its
		// standalone bytes even when capture first visited the non-inline edge.
		for _, origin := range snapshot.Metadata.Origins {
			raw, err := fetchEnvelopeBytes(ctx, store, origin.Descriptor)
			if err != nil {
				return err
			}
			envelope, err := bundle.ParseEnvelope(origin.Descriptor, raw)
			if err != nil {
				return err
			}
			for _, child := range envelope.Children {
				if child.Data != nil {
					if err := store.Push(ctx, child, bytes.NewReader(child.Data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
						return err
					}
				}
			}
		}
		state := localCapture{SchemaVersion: 1, Bundle: snapshot.Descriptor.Digest, Sources: map[digest.Digest]string{}}
		for dgst, d := range g.nodes {
			exists, err := store.Exists(ctx, d)
			if err != nil {
				return err
			}
			if exists {
				continue
			}
			if d.Data != nil {
				if err := store.Push(ctx, d, bytes.NewReader(d.Data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
					return err
				}
				continue
			}
			// Generated content has no published source and must never be omitted.
			generated, err := g.generated(ctx, d)
			if err != nil {
				return err
			}
			if generated {
				if err := copyCaptureObject(ctx, g, store, d); err != nil {
					return err
				}
				continue
			}
			state.Sources[dgst] = g.source[dgst]
		}
		if err := validateLocalCapture(ctx, store, snapshot, state); err != nil {
			return err
		}
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if len(raw) > maxCaptureBytes {
			return fmt.Errorf("local capture metadata exceeds %d bytes", maxCaptureBytes)
		}
		return os.WriteFile(filepath.Join(directory, captureFile), raw, 0600)
	})
	if err != nil {
		return result, err
	}
	result.LayoutPath = path
	s.progress(Progress{Operation: "created", Repository: path, Descriptor: result.Descriptor, Stats: result.Stats})
	return result, nil
}

type captureRecorder struct {
	source content.ReadOnlyStorage
	target *oci.Storage
}

func (r captureRecorder) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	if exists, err := r.target.Exists(ctx, d); err != nil {
		return nil, err
	} else if !exists {
		if err := copyCaptureObject(ctx, r.source, r.target, d); err != nil {
			return nil, err
		}
	}
	return r.target.Fetch(ctx, d)
}
func (r captureRecorder) Exists(ctx context.Context, d v1.Descriptor) (bool, error) {
	return r.source.Exists(ctx, d)
}
func copyCaptureObject(ctx context.Context, src content.Fetcher, dst content.Storage, d v1.Descriptor) (retErr error) {
	rc, err := src.Fetch(ctx, d)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, rc.Close()) }()
	verified := content.NewVerifyReader(rc, d)
	if err := dst.Push(ctx, d, verified); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return err
	}
	return verified.Verify()
}

func readLocalCapture(ctx context.Context, path string, store content.ReadOnlyStorage, snapshot bundle.Snapshot) (*localCapture, digest.Digest, error) {
	filename := filepath.Join(path, captureFile)
	entry, err := os.Lstat(filename)
	if os.IsNotExist(err) {
		return nil, "", fmt.Errorf("%w: local capture metadata disappeared", ErrNotFound)
	}
	if err != nil {
		return nil, "", err
	}
	if !entry.Mode().IsRegular() || entry.Size() > maxCaptureBytes {
		return nil, "", fmt.Errorf("invalid local capture metadata file")
	}
	fp, err := os.Open(filename)
	if os.IsNotExist(err) {
		return nil, "", fmt.Errorf("%w: local capture metadata disappeared", ErrNotFound)
	}
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = fp.Close() }()
	info, err := fp.Stat()
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxCaptureBytes {
		return nil, "", fmt.Errorf("invalid local capture metadata file")
	}
	raw, err := io.ReadAll(io.LimitReader(fp, maxCaptureBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) > maxCaptureBytes {
		return nil, "", fmt.Errorf("local capture metadata exceeds %d bytes", maxCaptureBytes)
	}
	if err := jsonutil.Validate(raw, true); err != nil {
		return nil, "", fmt.Errorf("local capture metadata: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var state localCapture
	if err := dec.Decode(&state); err != nil {
		return nil, "", err
	}
	if err := validateLocalCapture(ctx, store, snapshot, state); err != nil {
		return nil, "", err
	}
	return &state, digest.FromBytes(raw), nil
}

func validateLocalCapture(ctx context.Context, store content.ReadOnlyStorage, snapshot bundle.Snapshot, state localCapture) error {
	if state.SchemaVersion != 1 || state.Bundle != snapshot.Descriptor.Digest || state.Sources == nil {
		return fmt.Errorf("invalid local capture version, bundle or sources")
	}
	// All interpreted metadata has already been validated locally. Each locator
	// must be an opaque child of an original envelope at that physical repository.
	allowed := map[digest.Digest]map[string]v1.Descriptor{}
	requiresSource := map[digest.Digest]bool{}
	for _, origin := range snapshot.Metadata.Origins {
		raw, err := fetchEnvelopeBytes(ctx, store, origin.Descriptor)
		if err != nil {
			return err
		}
		env, err := bundle.ParseEnvelope(origin.Descriptor, raw)
		if err != nil {
			return err
		}
		for i, child := range env.Children {
			if !bundle.IsEnvelopeChild(origin.Descriptor, i, child) {
				if allowed[child.Digest] == nil {
					allowed[child.Digest] = map[string]v1.Descriptor{}
				}
				allowed[child.Digest][origin.Repository] = child
				if child.Data == nil {
					requiresSource[child.Digest] = true
				}
			}
		}
	}
	for dgst, repository := range state.Sources {
		ref, err := registry.ParseReference(repository)
		if err != nil || ref.Reference != "" || repositoryName(ref) != repository {
			return fmt.Errorf("invalid capture source repository %q", repository)
		}
		d, ok := allowed[dgst][repository]
		if !ok {
			return fmt.Errorf("capture source %s is not a reachable opaque child in %s", dgst, repository)
		}
		if exists, err := store.Exists(ctx, d); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("capture source %s is already retained locally", dgst)
		}
	}
	// Every omitted opaque child needs an explicit validated locator. Envelopes
	// and interpreted metadata cannot use this fallback, even after local damage.
	for dgst, repositories := range allowed {
		var d v1.Descriptor
		for _, candidate := range repositories {
			d = candidate
			break
		}
		exists, err := store.Exists(ctx, d)
		if err != nil {
			return err
		}
		if !exists && requiresSource[dgst] {
			if _, ok := state.Sources[dgst]; !ok {
				return fmt.Errorf("local capture has no source for %s", dgst)
			}
		}
	}
	return nil
}

type captureSource struct {
	local   content.ReadOnlyStorage
	service *Service
	sources map[digest.Digest]string
	stats   *Stats
}

func (s captureSource) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	if exists, err := s.local.Exists(ctx, d); err != nil {
		return nil, err
	} else if exists {
		return s.local.Fetch(ctx, d)
	}
	repository, ok := s.sources[d.Digest]
	if !ok {
		return nil, fmt.Errorf("%w: missing local capture metadata %s", ErrNotFound, d.Digest)
	}
	repo, err := s.service.repository(repository)
	if err != nil {
		return nil, err
	}
	rc, err := repo.Blobs().Fetch(ctx, d)
	if err == nil && s.stats != nil {
		s.stats.BlobBytesDownloaded += d.Size
	}
	return rc, err
}
func (s captureSource) Exists(ctx context.Context, d v1.Descriptor) (bool, error) {
	if exists, err := s.local.Exists(ctx, d); err != nil || exists {
		return exists, err
	}
	_, ok := s.sources[d.Digest]
	return ok, nil
}

// openLocalBundle never silently turns a damaged complete layout into an
// online source. Remote payload fallback requires the explicit capture marker.
func (s *Service) openLocalBundle(ctx context.Context, path string) (content.ReadOnlyStorage, bundle.Snapshot, *localCapture, digest.Digest, error) {
	store, root, err := readLayoutRoot(path)
	if err != nil {
		return nil, bundle.Snapshot{}, nil, "", err
	}
	_, err = os.Lstat(filepath.Join(path, captureFile))
	if os.IsNotExist(err) {
		snapshot, err := bundle.LoadSnapshotGraph(ctx, store, root)
		return store, snapshot, nil, "", err
	}
	if err != nil {
		return nil, bundle.Snapshot{}, nil, "", err
	}
	snapshot, err := bundle.InspectSnapshotGraph(ctx, store, root)
	if err != nil {
		return nil, bundle.Snapshot{}, nil, "", err
	}
	state, hash, err := readLocalCapture(ctx, path, store, snapshot)
	return store, snapshot, state, hash, err
}
