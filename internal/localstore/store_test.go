package localstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

func put(t *testing.T, s content.Storage, media string, raw []byte) v1.Descriptor {
	t.Helper()
	d := content.NewDescriptorFromBytes(media, raw)
	if err := s.Push(t.Context(), d, bytes.NewReader(raw)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		t.Fatal(err)
	}
	return d
}
func member(t *testing.T, s content.Storage, payload string) v1.Descriptor {
	t.Helper()
	empty := put(t, s, v1.MediaTypeEmptyJSON, []byte("{}"))
	layer := put(t, s, "application/example", []byte(payload))
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: empty, Layers: []v1.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	return put(t, s, v1.MediaTypeImageManifest, raw)
}
func snapshot(t *testing.T, s content.Storage, root v1.Descriptor, repo string) bundle.Snapshot {
	t.Helper()
	policy := dependency.Policy{Follow: "none"}
	metadata := bundle.Metadata{SchemaVersion: 1, Recipe: config.Recipe{Include: []config.Selection{{Ref: repo + ":stable", Referrers: config.Policy{Include: "none", Require: []config.RequiredReferrer{}}, Dependencies: policy}}}, Roots: []bundle.Root{{Repository: repo, Reference: "stable", Descriptor: root}}, Origins: []bundle.Origin{{Repository: repo, Descriptor: root}}, Referrers: []bundle.Association{}, Dependencies: []bundle.DependencyResolution{{Repository: repo, Subject: root, Policy: policy, Outcome: "disabled", Dependencies: []bundle.ResolvedDependency{}}}}
	result, err := bundle.BuildSnapshot(t.Context(), s, metadata)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func fixture(t *testing.T, s *Store, repo string, tags ...string) bundle.Snapshot {
	t.Helper()
	var result bundle.Snapshot
	if err := s.With(t.Context(), func(session *Session) error {
		result = snapshot(t, session.Storage(), member(t, session.Storage(), "shared payload"), repo)
		return session.Publish(t.Context(), result.Descriptor, nil, tags)
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDefaultPathAndNames(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	path, err := DefaultPath()
	if err != nil || path != filepath.Join(os.Getenv("XDG_DATA_HOME"), "bale") {
		t.Fatalf("default: %s %v", path, err)
	}
	t.Setenv("XDG_DATA_HOME", "relative")
	if _, err := DefaultPath(); err == nil {
		t.Fatal("relative XDG accepted")
	}
	for value, want := range map[string]string{"release": "localhost/release:latest", "release:v1": "localhost/release:v1", "team/release": "localhost/team/release:latest", "registry.example/team/release:v1": "registry.example/team/release:v1"} {
		got, err := NormalizeTag(value)
		if err != nil || got != want {
			t.Fatalf("normalize %q: %q %v", value, got, err)
		}
	}
	for _, value := range []string{"", "bad name", "release@sha256:bad", "https://registry.example/name", digest.FromString("root").String()} {
		if _, err := NormalizeTag(value); err == nil {
			t.Fatalf("invalid name accepted: %q", value)
		}
	}
}

func TestRetagRetainsUnnamedRootsAndRemovalIsReferenceOnly(t *testing.T) {
	s := newStore(t)
	first := fixture(t, s, "source.example/one", "release:v1")
	second := fixture(t, s, "source.example/two", "release:v1")
	err := s.With(t.Context(), func(session *Session) error {
		rows, err := session.List(t.Context())
		if err != nil {
			return err
		}
		if len(rows) != 2 {
			t.Fatalf("retag lost root: %+v", rows)
		}
		original, err := session.Resolve(t.Context(), first.Descriptor.Digest.String())
		if err != nil || len(original.Names) != 0 {
			t.Fatalf("displaced root not retained: %+v %v", original, err)
		}
		named, err := session.Resolve(t.Context(), "release:v1")
		if err != nil || named.Descriptor.Digest != second.Descriptor.Digest {
			t.Fatalf("wrong alias: %+v %v", named, err)
		}
		if err := session.Tag(t.Context(), "release:v1", []string{"registry.example/release:approved"}); err != nil {
			return err
		}
		if err := session.Remove(t.Context(), []string{"release:v1"}); err != nil {
			return err
		}
		if _, err := session.Resolve(t.Context(), second.Descriptor.Digest.String()); err != nil {
			return err
		}
		if err := session.Remove(t.Context(), []string{"registry.example/release:approved"}); err != nil {
			return err
		}
		if _, err := session.Resolve(t.Context(), second.Descriptor.Digest.String()); !errors.Is(err, errdef.ErrNotFound) {
			t.Fatalf("last alias did not remove root: %v", err)
		}
		exists, err := session.Storage().Exists(t.Context(), second.Descriptor)
		if err != nil || !exists {
			t.Fatal("rm deleted blob bytes")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if err := s.With(t.Context(), func(session *Session) error {
		_, err := session.Resolve(t.Context(), first.Descriptor.Digest.String())
		return err
	}); err != nil {
		t.Fatal("prune discarded unnamed root:", err)
	}
}

func TestPrunePreservesNestedRootsAndSharedBlobs(t *testing.T) {
	s := newStore(t)
	child := fixture(t, s, "source.example/child", "child")
	var parent bundle.Snapshot
	if err := s.With(t.Context(), func(session *Session) error {
		parent = snapshot(t, session.Storage(), child.Descriptor, "holding.example/nested")
		if err := session.Publish(t.Context(), parent.Descriptor, nil, []string{"parent"}); err != nil {
			return err
		}
		return session.Remove(t.Context(), []string{child.Descriptor.Digest.String()})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if err := s.With(t.Context(), func(session *Session) error {
		if _, err := bundle.LoadSnapshotGraph(t.Context(), session.Storage(), parent.Descriptor); err != nil {
			return err
		}
		return session.Remove(t.Context(), []string{"parent"})
	}); err != nil {
		t.Fatal(err)
	}
	preview, err := s.Prune(t.Context(), true)
	if err != nil || preview.Blobs == 0 || preview.Bytes == 0 {
		t.Fatalf("empty prune preview: %+v %v", preview, err)
	}
	actual, err := s.Prune(t.Context(), false)
	if err != nil || actual != preview {
		t.Fatalf("prune mismatch: %+v %+v %v", preview, actual, err)
	}
}

func TestPruneWaitsForActiveOperationsAndCancellation(t *testing.T) {
	s := newStore(t)
	fixture(t, s, "source.example/root", "root")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() { done <- s.With(t.Context(), func(*Session) error { close(entered); <-release; return nil }) }()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Millisecond)
	defer cancel()
	if _, err := s.Prune(ctx, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("prune ignored active lease: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	if err := s.With(ctx, func(*Session) error { t.Fatal("entered canceled operation"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestConcurrentProcessesRetainAllAliases(t *testing.T) {
	if path := os.Getenv("BALE_STORE_TEST_PATH"); path != "" {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 8; i++ {
			name := fmt.Sprintf("worker%s/name%d", os.Getenv("BALE_STORE_TEST_WORKER"), i)
			if err := s.With(t.Context(), func(session *Session) error {
				return session.Tag(t.Context(), os.Getenv("BALE_STORE_TEST_ROOT"), []string{name})
			}); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	s := newStore(t)
	root := fixture(t, s, "source.example/root")
	var group sync.WaitGroup
	for i := 0; i < 4; i++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConcurrentProcessesRetainAllAliases$")
			cmd.Env = append(os.Environ(), "BALE_STORE_TEST_PATH="+s.Path(), "BALE_STORE_TEST_ROOT="+root.Descriptor.Digest.String(), fmt.Sprintf("BALE_STORE_TEST_WORKER=%d", worker))
			if raw, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("child process: %v\n%s", err, raw)
			}
		}(i)
	}
	group.Wait()
	if err := s.With(t.Context(), func(session *Session) error {
		row, err := session.Resolve(t.Context(), root.Descriptor.Digest.String())
		if err != nil {
			return err
		}
		if len(row.Names) != 32 {
			t.Fatalf("lost concurrent aliases: %d", len(row.Names))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledPublicationDoesNotExposeRoot(t *testing.T) {
	s := newStore(t)
	if err := s.With(t.Context(), func(session *Session) error {
		root := snapshot(t, session.Storage(), member(t, session.Storage(), "new payload"), "source.example/new")
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		if err := session.Publish(canceled, root.Descriptor, nil, []string{"new"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled publish: %v", err)
		}
		rows, err := session.List(t.Context())
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			t.Fatal("canceled publication exposed root")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMissingIndexFailsClosedBeforeOpenOrPrune(t *testing.T) {
	s := newStore(t)
	root := fixture(t, s, "source.example/retained", "retained")
	blob := filepath.Join(s.Path(), "blobs", root.Descriptor.Digest.Algorithm().String(), root.Descriptor.Digest.Encoded())
	if err := os.Remove(filepath.Join(s.Path(), "index.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(s.Path()); err == nil {
		t.Fatal("opened populated store without its retention index")
	}
	if _, err := s.Prune(t.Context(), false); err == nil {
		t.Fatal("pruned after retention index loss")
	}
	if _, err := os.Stat(blob); err != nil {
		t.Fatal("index loss deleted retained content:", err)
	}
	if _, err := os.Stat(filepath.Join(s.Path(), "index.json")); !os.IsNotExist(err) {
		t.Fatal("lost index was silently replaced")
	}
}

func TestOpenContextCancelsBehindExclusiveActivity(t *testing.T) {
	s := newStore(t)
	held, err := lock(t.Context(), filepath.Join(s.Path(), ".activity.lock"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := OpenContext(ctx, s.Path()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("open ignored cancellation: %v", err)
	}
}
