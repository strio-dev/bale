package localstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	digest "github.com/opencontainers/go-digest"
)

func writeStoreFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRecoversInterruptedFirstInitialization(t *testing.T) {
	path := t.TempDir()
	// State after initialization publishes oci-layout but before it writes index.json.
	const marker = "{\n  \"imageLayoutVersion\": \"1.0.0\"\n}\n"
	layout := filepath.Join(path, "oci-layout")
	writeStoreFixture(t, layout, marker)
	writeStoreFixture(t, filepath.Join(path, ".activity.lock"), "")
	writeStoreFixture(t, filepath.Join(path, ".metadata.lock"), "")

	s, err := OpenContext(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.With(t.Context(), func(session *Session) error {
		entries, err := session.List(t.Context())
		if err == nil && len(entries) != 0 {
			t.Fatalf("recovered index is not empty: %+v", entries)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fixture(t, s, "source.example/recovered", "recovered")
	if _, err := Open(s.Path()); err != nil {
		t.Fatal("could not reopen recovered store:", err)
	}
	if raw, err := os.ReadFile(layout); err != nil || string(raw) != marker {
		t.Fatalf("layout marker was rewritten: %q, %v", raw, err)
	}
}

func TestOpenRejectsNonPristineOrInvalidLayoutWithoutIndex(t *testing.T) {
	const valid = `{"imageLayoutVersion":"1.0.0"}`
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{"unknown", func(t *testing.T, dir string) {
			writeStoreFixture(t, filepath.Join(dir, "notes"), "preserve")
		}},
		{"index write in progress", func(t *testing.T, dir string) {
			writeStoreFixture(t, filepath.Join(dir, ".bale-write-123"), "{")
		}},
		{"empty blobs", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "blobs"), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"corrupt layout", func(t *testing.T, dir string) {
			writeStoreFixture(t, filepath.Join(dir, "oci-layout"), "{")
		}},
		{"duplicate layout version", func(t *testing.T, dir string) {
			writeStoreFixture(t, filepath.Join(dir, "oci-layout"), `{"imageLayoutVersion":"0.9.0","imageLayoutVersion":"1.0.0"}`)
		}},
		{"extra layout field", func(t *testing.T, dir string) {
			writeStoreFixture(t, filepath.Join(dir, "oci-layout"), `{"imageLayoutVersion":"1.0.0","other":"data"}`)
		}},
		{"unsupported layout version", func(t *testing.T, dir string) {
			writeStoreFixture(t, filepath.Join(dir, "oci-layout"), `{"imageLayoutVersion":"2.0.0"}`)
		}},
		{"symlink layout", func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "layout")
			writeStoreFixture(t, target, valid)
			layout := filepath.Join(dir, "oci-layout")
			if err := os.Remove(layout); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, layout); err != nil {
				t.Fatal(err)
			}
		}},
		{"layout directory", func(t *testing.T, dir string) {
			layout := filepath.Join(dir, "oci-layout")
			if err := os.Remove(layout); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(layout, 0700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeStoreFixture(t, filepath.Join(dir, "oci-layout"), valid)
			tc.setup(t, dir)
			if _, err := Open(dir); err == nil {
				t.Fatal("opened unrecognized store without a retention index")
			}
			assertMissingStoreFile(t, filepath.Join(dir, "index.json"))
		})
	}
}

func assertMissingStoreFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected %s to be absent: %v", path, err)
	}
}

func TestPruneToleratesInterruptedCaptureWrite(t *testing.T) {
	s := newStore(t)
	root := fixture(t, s, "source.example/retained", "retained")
	var unretained digest.Digest
	if err := s.With(t.Context(), func(session *Session) error {
		unretained = put(t, session.Storage(), "application/octet-stream", []byte("unretained")).Digest
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	candidate := capturePath(s.Path(), unretained)
	dir := filepath.Dir(candidate)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeStoreFixture(t, candidate, "{}")
	temp, err := os.CreateTemp(dir, ".bale-write-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temp.WriteString("{"); err != nil {
		_ = temp.Close()
		t.Fatal(err)
	}
	if err := temp.Close(); err != nil {
		t.Fatal(err)
	}

	preview, err := s.Prune(t.Context(), true)
	if err != nil || preview.Blobs != 1 || preview.Captures != 1 {
		t.Fatalf("wrong prune preview: %+v, %v", preview, err)
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatal("dry run removed capture:", err)
	}
	actual, err := s.Prune(t.Context(), false)
	if err != nil || actual != preview {
		t.Fatalf("real prune differs from preview: %+v vs %+v, %v", actual, preview, err)
	}
	assertMissingStoreFile(t, candidate)
	if raw, err := os.ReadFile(temp.Name()); err != nil || string(raw) != "{" {
		t.Fatalf("prune modified temporary file of unverified provenance: %q, %v", raw, err)
	}
	if err := s.With(t.Context(), func(session *Session) error {
		record, err := session.Resolve(t.Context(), "retained")
		if err == nil && record.Descriptor.Digest != root.Descriptor.Digest {
			t.Errorf("retained reference changed: %s", record.Descriptor.Digest)
		}
		return err
	}); err != nil {
		t.Fatal("prune damaged a retained root:", err)
	}
	if again, err := s.Prune(t.Context(), true); err != nil || again != (PruneResult{}) {
		t.Fatalf("prune did not finish collecting: %+v, %v", again, err)
	}
}

func TestPruneRejectsUnknownCaptureEntriesBeforeDeletion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry string
		setup func(*testing.T, string)
	}{
		{"unknown", "notes", func(t *testing.T, path string) { writeStoreFixture(t, path, "keep") }},
		{"similar prefix", ".bale-write-notes", func(t *testing.T, path string) { writeStoreFixture(t, path, "keep") }},
		{"noncanonical suffix", ".bale-write-001", func(t *testing.T, path string) { writeStoreFixture(t, path, "keep") }},
		{"writer-shaped directory", ".bale-write-42", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"writer-shaped symlink", ".bale-write-42", func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "target")
			writeStoreFixture(t, target, "keep")
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"writer-shaped public file", ".bale-write-42", func(t *testing.T, path string) {
			writeStoreFixture(t, path, "keep")
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			candidate := capturePath(s.Path(), digest.FromString("unretained"))
			if err := os.MkdirAll(filepath.Dir(candidate), 0700); err != nil {
				t.Fatal(err)
			}
			writeStoreFixture(t, candidate, "{}")
			foreign := filepath.Join(filepath.Dir(candidate), tc.entry)
			tc.setup(t, foreign)

			for _, dryRun := range []bool{true, false} {
				if result, err := s.Prune(t.Context(), dryRun); err == nil || result != (PruneResult{}) {
					t.Fatalf("prune(dryRun=%v) accepted unknown entry: %+v, %v", dryRun, result, err)
				}
			}
			for _, path := range []string{candidate, foreign} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("prune removed %s: %v", path, err)
				}
			}
		})
	}
}

func TestPrunePreflightsCaptureFilesBeforeDeletingBlobs(t *testing.T) {
	s := newStore(t)
	fixture(t, s, "source.example/retained", "retained")
	var unretained digest.Digest
	if err := s.With(t.Context(), func(session *Session) error {
		unretained = put(t, session.Storage(), "application/octet-stream", []byte("unretained")).Digest
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	capture := capturePath(s.Path(), unretained)
	if err := os.MkdirAll(capture, 0700); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(s.Path(), "blobs", unretained.Algorithm().String(), unretained.Encoded())
	for _, dryRun := range []bool{true, false} {
		if result, err := s.Prune(t.Context(), dryRun); err == nil || result != (PruneResult{}) {
			t.Fatalf("prune(dryRun=%v) accepted capture directory: %+v, %v", dryRun, result, err)
		}
		if _, err := os.Stat(blob); err != nil {
			t.Fatal("prune deleted unretained blob before capture validation:", err)
		}
		if info, err := os.Stat(capture); err != nil || !info.IsDir() {
			t.Fatalf("prune changed capture directory: %v, %v", info, err)
		}
	}
}

func TestOpenInitializedStoreWithContainedMarkerAlias(t *testing.T) {
	s := newStore(t)
	root := fixture(t, s, "source.example/retained", "retained")
	marker := filepath.Join(s.Path(), "oci-layout")
	target := filepath.Join(s.Path(), "layout-marker.json")
	if err := os.Rename(marker, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(target), marker); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			t.Skipf("creating symlinks is not permitted: %v", err)
		}
		t.Fatal(err)
	}
	reopened, err := Open(s.Path())
	if err != nil {
		t.Fatal("reopen initialized store with contained marker alias:", err)
	}
	if err := reopened.With(t.Context(), func(session *Session) error {
		entries, err := session.List(t.Context())
		if err != nil {
			return err
		}
		if len(entries) != 1 || entries[0].Descriptor.Digest != root.Descriptor.Digest ||
			len(entries[0].Names) != 1 || entries[0].Names[0] != "localhost/retained:latest" {
			t.Fatalf("contained marker alias changed retained bundle: %+v", entries)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
