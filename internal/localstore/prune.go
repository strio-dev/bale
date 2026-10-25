package localstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
)

// Prune removes only blobs unreachable from all retained roots, including
// unnamed roots. Exclusive activity blocks ingestion and active graph readers.
func (s *Store) Prune(ctx context.Context, dryRun bool) (result PruneResult, retErr error) {
	fp, err := lock(ctx, filepath.Join(s.path, ".activity.lock"), true)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, fp.Close()) }()
	session := &Session{store: s}
	retErr = session.metadata(ctx, func(current *index) error {
		marked := map[digest.Digest]bool{}
		// Finish validating every retained graph before deleting any content.
		for id, root := range current.roots {
			if _, err := session.record(ctx, current, id); err != nil {
				return err
			}
			_, inventory, err := bundle.InspectSnapshotInventory(ctx, s.storage, root)
			if err != nil {
				return err
			}
			for d := range inventory {
				marked[d] = true
			}
		}
		blobs, err := os.ReadDir(filepath.Join(s.path, "blobs"))
		if os.IsNotExist(err) {
			blobs = nil
		} else if err != nil {
			return err
		}
		candidates := []v1.Descriptor{}
		for _, algorithm := range blobs {
			if !algorithm.IsDir() {
				return fmt.Errorf("invalid blob algorithm directory")
			}
			files, err := os.ReadDir(filepath.Join(s.path, "blobs", algorithm.Name()))
			if err != nil {
				return err
			}
			for _, file := range files {
				d, err := digest.Parse(algorithm.Name() + ":" + file.Name())
				if err != nil {
					return fmt.Errorf("invalid stored blob path: %w", err)
				}
				if marked[d] {
					continue
				}
				info, err := file.Info()
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() {
					return fmt.Errorf("invalid stored blob file")
				}
				candidates = append(candidates, v1.Descriptor{Digest: d, Size: info.Size(), MediaType: "application/octet-stream"})
			}
		}
		captures := []string{}
		algorithms, err := os.ReadDir(filepath.Join(s.path, "captures"))
		if !os.IsNotExist(err) && err != nil {
			return err
		}
		for _, algorithm := range algorithms {
			if !algorithm.IsDir() {
				return fmt.Errorf("invalid capture directory")
			}
			files, err := os.ReadDir(filepath.Join(s.path, "captures", algorithm.Name()))
			if err != nil {
				return err
			}
			for _, file := range files {
				if !strings.HasSuffix(file.Name(), ".json") {
					return fmt.Errorf("invalid capture record path")
				}
				d, err := digest.Parse(algorithm.Name() + ":" + strings.TrimSuffix(file.Name(), ".json"))
				if err != nil {
					return err
				}
				if _, retained := current.roots[d]; !retained {
					captures = append(captures, filepath.Join(s.path, "captures", algorithm.Name(), file.Name()))
				}
			}
		}
		for _, d := range candidates {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !dryRun {
				if err := s.storage.Delete(ctx, d); err != nil {
					return err
				}
			}
			result.Blobs++
			result.Bytes += d.Size
		}
		for _, path := range captures {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !dryRun {
				if err := os.Remove(path); err != nil {
					return err
				}
			}
			result.Captures++
		}
		return nil
	})
	return
}
