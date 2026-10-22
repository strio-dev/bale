package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
)

func (s *Service) Pull(ctx context.Context, bundleRef, layoutPath string, mode PullMode) (PullResult, error) {
	return s.PullWithOptions(ctx, bundleRef, layoutPath, mode, PullOptions{})
}

func (s *Service) PullWithOptions(ctx context.Context, bundleRef, layoutPath string, mode PullMode, options PullOptions) (result PullResult, retErr error) {
	result = PullResult{Mode: mode, Stats: Stats{Accounting: statsAccounting}}
	defer func() {
		if retErr != nil {
			retErr = operationError("pull", bundleRef, result.Descriptor, nil, retErr)
		}
	}()
	var captured *graph
	defer func() {
		result.Stats.BlobBytesDownloaded = result.Stats.BlobBytesUploaded
		result.Stats.ManifestBytesDownloaded = result.Stats.ManifestBytesUploaded
		result.Stats.BlobBytesUploaded = 0
		result.Stats.ManifestBytesUploaded = 0
		if captured != nil {
			result.Stats.BlobBytesDownloaded = captured.stats.BlobBytesDownloaded
			result.Stats.ManifestBytesDownloaded = captured.stats.ManifestBytesDownloaded
		}
		if retErr == nil && result.LayoutPath != "" {
			s.progress(Progress{Operation: "pulled", Repository: bundleRef, Descriptor: result.Descriptor, Stats: result.Stats})
		}
	}()
	if mode != PullLocked && mode != PullLatest {
		return result, fmt.Errorf("%w: pull mode must be locked or latest", ErrInvalid)
	}
	if err := checkLayoutDestination(layoutPath); err != nil {
		return result, err
	}
	ref, err := parseRef(bundleRef)
	if err != nil {
		return result, operationError("reference", bundleRef, v1.Descriptor{}, ErrInvalid, err)
	}
	repo, err := s.repository(repositoryName(ref))
	if err != nil {
		return result, err
	}
	root, err := repo.Resolve(ctx, ref.Reference)
	if err != nil {
		return result, err
	}
	var snapshot bundle.Snapshot
	var src content.Fetcher = repo
	if mode == PullLatest {
		snapshot, err = bundle.InspectSnapshot(ctx, repo, root)
		if err != nil {
			return result, err
		}
		if recipeHasPlugins(snapshot.Metadata.Recipe) && !options.AllowPlugins {
			return result, fmt.Errorf("%w: latest refresh includes plugins; explicit execution permission is required", ErrInvalid)
		}
		captured, snapshot, err = s.capture(ctx, snapshot.Metadata.Recipe)
		if err != nil {
			return result, err
		}
		root = snapshot.Descriptor
		src = captured
	}
	result.Descriptor = root
	if err := writeLayout(ctx, src, root, layoutPath, &result.Stats); err != nil {
		return result, operationError("layout", layoutPath, root, nil, err)
	}
	result.LayoutPath = layoutPath
	return result, nil
}

// checkLayoutDestination rejects every existing filesystem entry, including a
// dangling symlink. Publication repeats this check atomically at rename time.
func checkLayoutDestination(layoutPath string) error {
	if layoutPath == "" {
		return fmt.Errorf("%w: layout path is required", ErrInvalid)
	}
	if _, err := os.Lstat(layoutPath); err == nil {
		return fmt.Errorf("%w: layout path already exists: %s", ErrConflict, layoutPath)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// writeLayout publishes one verified bundle closure in a new OCI layout. Source
// content is copied into a sibling temporary directory; no partial layout is
// exposed, and an existing destination is never replaced.
func writeLayout(ctx context.Context, src content.Fetcher, root v1.Descriptor, layoutPath string, stats *Stats) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkLayoutDestination(layoutPath); err != nil {
		return err
	}
	parent := filepath.Dir(filepath.Clean(layoutPath))
	if err := os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(parent, ".bale-layout-")
	if err != nil {
		return err
	}
	defer func() {
		if temp != "" {
			retErr = errors.Join(retErr, os.RemoveAll(temp))
		}
	}()
	store, err := oci.NewStorage(temp)
	if err != nil {
		return err
	}
	if err := copyGraph(ctx, src, store, root, stats, nil); err != nil {
		return err
	}
	if _, err := bundle.LoadSnapshotGraph(ctx, store, root); err != nil {
		return err
	}
	raw, err := json.Marshal(v1.ImageLayout{Version: v1.ImageLayoutVersion})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(temp, "oci-layout"), raw, 0644); err != nil {
		return err
	}
	// Exactly one completion descriptor, written only after closure validation.
	raw, err = json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{root}})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(temp, "index.json"), raw, 0644); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publishLayoutDirectory(temp, layoutPath); err != nil {
		return err
	}
	temp = ""
	return nil
}

func openLayout(ctx context.Context, path string) (*oci.Storage, bundle.Snapshot, error) {
	raw, err := os.ReadFile(filepath.Join(path, "oci-layout"))
	if err != nil {
		return nil, bundle.Snapshot{}, err
	}
	var layout v1.ImageLayout
	if err := json.Unmarshal(raw, &layout); err != nil || layout.Version != v1.ImageLayoutVersion {
		return nil, bundle.Snapshot{}, fmt.Errorf("invalid OCI layout")
	}
	raw, err = os.ReadFile(filepath.Join(path, "index.json"))
	if err != nil {
		return nil, bundle.Snapshot{}, err
	}
	var index v1.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return nil, bundle.Snapshot{}, err
	}
	if index.SchemaVersion != 2 || len(index.Manifests) != 1 {
		return nil, bundle.Snapshot{}, fmt.Errorf("layout must contain exactly one bundle root")
	}
	store, err := oci.NewStorage(path)
	if err != nil {
		return nil, bundle.Snapshot{}, err
	}
	snapshot, err := bundle.LoadSnapshotGraph(ctx, store, index.Manifests[0])
	if err != nil {
		return nil, bundle.Snapshot{}, err
	}
	return store, snapshot, nil
}
