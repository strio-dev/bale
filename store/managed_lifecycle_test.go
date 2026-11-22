package store_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/strio-dev/bale/store"
)

func TestManagedReferencesAndPrunePreserveSharedGraph(t *testing.T) {
	f, server, client, recipe := source(t)
	defer server.Close()
	ctx := t.Context()
	s := local(t, client)
	first, err := s.Create(ctx, recipe, store.CreateOptions{Tags: []string{"release:current"}})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.tag = f.artifact(t, "two")
	f.mu.Unlock()
	second, err := s.Create(ctx, recipe, store.CreateOptions{Tags: []string{"release:current", "release:keep"}})
	if err != nil {
		t.Fatal(err)
	}
	firstID, secondID := first.Descriptor.Digest, second.Descriptor.Digest
	if firstID == secondID || !first.MetadataOnly || !second.MetadataOnly {
		t.Fatalf("expected two distinct metadata-only roots: %s %s", firstID, secondID)
	}

	// Retagging must not change immutable, even now unnamed, roots.
	shortFirstID := firstID.Encoded()[:12]
	for _, lookup := range []struct {
		reference string
		want      digest.Digest
	}{
		{"release:current", secondID},
		{"release@" + firstID.String(), firstID},
		{shortFirstID, firstID},
	} {
		got, err := s.Inspect(ctx, lookup.reference)
		if err != nil || !got.MetadataOnly || got.Descriptor.Digest != lookup.want {
			t.Fatalf("inspect %q: digest=%s metadataOnly=%v err=%v", lookup.reference, got.Descriptor.Digest, got.MetadataOnly, err)
		}
	}
	if diff, err := s.Diff(ctx, shortFirstID, "release:current"); err != nil || !diff.Changed {
		t.Fatalf("metadata diff missed retagged root: changed=%v err=%v", diff.Changed, err)
	}
	if got := f.gets.Load(); got != 0 {
		t.Fatalf("metadata lookups downloaded payload %d times", got)
	}

	if _, err := s.Materialize(ctx, shortFirstID); err != nil {
		t.Fatal(err)
	}
	server.Close()
	if err := s.Remove(ctx, []string{firstID.String(), "release:current"}); err != nil {
		t.Fatal(err)
	}
	preview, err := s.Prune(ctx, true)
	if err != nil || preview.Blobs == 0 || preview.Bytes == 0 {
		t.Fatalf("prune preview did not find removed root's bytes: %+v %v", preview, err)
	}
	// A dry run must not remove the bytes reported by the real run.
	pruned, err := s.Prune(ctx, false)
	if err != nil || pruned != preview {
		t.Fatalf("prune did not match preview: got %+v want %+v err=%v", pruned, preview, err)
	}
	// Saving the retained alias offline verifies that shared bytes survived GC.
	saved, err := s.SaveTo(ctx, "release:keep", io.Discard)
	if err != nil || saved.Descriptor.Digest != secondID {
		t.Fatalf("shared graph did not survive prune: %s %v", saved.Descriptor.Digest, err)
	}
	if got := f.gets.Load(); got != 1 {
		t.Fatalf("shared payload GETs = %d, want 1", got)
	}
}

type heldArchiveWriter struct {
	started chan struct{}
	release chan struct{}
}

func (w *heldArchiveWriter) Write(p []byte) (int, error) {
	if w.started != nil {
		close(w.started)
		w.started = nil
		<-w.release
	}
	return len(p), nil
}

func TestManagedSaveToHoldsPruneLease(t *testing.T) {
	_, server, client, recipe := source(t)
	defer server.Close()
	s := local(t, client)
	created, err := s.Create(t.Context(), recipe, store.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Descriptor.Digest.String()
	if _, err := s.Materialize(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	server.Close()

	saveCtx, cancelSave := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelSave()
	started := make(chan struct{})
	writer := &heldArchiveWriter{started: started, release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := s.SaveTo(saveCtx, id, writer)
		done <- err
	}()
	pruneErr := func() error {
		defer close(writer.release)
		select {
		case <-started:
		case err := <-done:
			t.Fatalf("archive finished before writing: %v", err)
		case <-saveCtx.Done():
			t.Fatalf("waiting for archive writer: %v", saveCtx.Err())
		}
		wait, cancel := context.WithTimeout(saveCtx, 100*time.Millisecond)
		defer cancel()
		_, err := s.Prune(wait, true)
		return err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("save under prune contention: %v", err)
		}
	case <-saveCtx.Done():
		t.Fatalf("waiting for archive completion: %v", saveCtx.Err())
	}
	if !errors.Is(pruneErr, context.DeadlineExceeded) {
		t.Fatalf("prune did not wait for the active archive read: %v", pruneErr)
	}
	if _, err := s.Prune(t.Context(), false); err != nil {
		t.Fatalf("prune after archive finished: %v", err)
	}
}
