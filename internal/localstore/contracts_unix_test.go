//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package localstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestOverlappingStoreSessionsKeepPruneExcluded(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := s.With(ctx, func(*Session) error {
		// Both leases can be held together. Closing the second must not
		// release the first, even though they belong to the same process.
		if err := s.With(ctx, func(*Session) error { return nil }); err != nil {
			return fmt.Errorf("could not share active session: %w", err)
		}
		wait, cancel := context.WithTimeout(ctx, 35*time.Millisecond)
		defer cancel()
		if _, err := s.Prune(wait, true); !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("prune did not wait for the first lease: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(ctx, true); err != nil {
		t.Fatal("prune after both leases closed:", err)
	}
}

func TestCanceledMetadataWaitLeavesAliasesUnchanged(t *testing.T) {
	s := newStore(t)
	fixture(t, s, "source.example/root", "root")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := s.With(ctx, func(session *Session) error {
		if err := session.metadata(ctx, func(*index) error {
			wait, cancel := context.WithTimeout(ctx, 35*time.Millisecond)
			defer cancel()
			if err := session.Tag(wait, "root", []string{"queued"}); !errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("tag did not wait for metadata: %v", err)
			}
			return nil
		}); err != nil {
			return err
		}
		if err := session.Tag(ctx, "root", []string{"released"}); err != nil {
			return fmt.Errorf("tag after metadata lock released: %w", err)
		}
		record, err := session.Resolve(ctx, "released")
		if err == nil && !slices.Equal(record.Names, []string{"localhost/released:latest", "localhost/root:latest"}) {
			return fmt.Errorf("unexpected aliases after canceled tag: %v", record.Names)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicWriteReplacesFileWithoutChangingOpenReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata", "entry.json")
	const original, replacement = `{"value":"old"}`, `{"value":"new metadata"}`
	if err := atomicWrite(path, []byte(original)); err != nil {
		t.Fatal(err)
	}
	before, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = before.Close() }()
	if err := atomicWrite(path, []byte(replacement)); err != nil {
		t.Fatal(err)
	}
	if raw, err := io.ReadAll(before); err != nil || string(raw) != original {
		t.Fatalf("existing reader saw %q, %v; want %q", raw, err, original)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != replacement {
		t.Fatalf("new reader saw %q, %v; want %q", raw, err, replacement)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode(); !mode.IsRegular() || mode.Perm()&^0600 != 0 {
		t.Fatalf("replacement is not a private regular file: %v", mode)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("unexpected files after replacement: %v, %v", entries, err)
	}
}
