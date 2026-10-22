package transport

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func TestArchiveRoundTripDeterministicClosure(t *testing.T) {
	src := memory.New()
	child := snapshotFor(t, src, []string{"example.test/image"}, []v1.Descriptor{object(t, src, "shared payload")})
	root := snapshotFor(t, src, []string{"example.test/bundle", "example.test/other"}, []v1.Descriptor{child.Descriptor, child.Descriptor})
	layout := layoutFor(t, src, root.Descriptor)
	if err := os.WriteFile(filepath.Join(layout, "secret"), []byte("do not archive"), 0600); err != nil {
		t.Fatal(err)
	}
	// An unrelated valid blob must be excluded as well as arbitrary files.
	if err := os.WriteFile(filepath.Join(layout, "blobs", "sha256", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	svc := &Service{}
	first := filepath.Join(t.TempDir(), "first.tar")
	result, err := svc.Save(ctx, layout, first)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Descriptor, root.Descriptor) || result.ArchivePath != first || result.LayoutPath != layout {
		t.Fatalf("save result: %+v", result)
	}
	second := filepath.Join(t.TempDir(), "second.tar")
	if _, err := svc.Save(ctx, layout, second); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(first)
	b, _ := os.ReadFile(second)
	if !bytes.Equal(a, b) {
		t.Fatal("archive bytes are not deterministic")
	}
	tr := tar.NewReader(bytes.NewReader(a))
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if seen[h.Name] {
			t.Fatalf("duplicate %s", h.Name)
		}
		seen[h.Name] = true
		if h.Name == "secret" || h.Name == "blobs/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
			t.Fatalf("unrelated member %s", h.Name)
		}
	}
	loaded := filepath.Join(t.TempDir(), "loaded")
	got, err := svc.Load(ctx, first, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Descriptor, root.Descriptor) || got.LayoutPath != loaded || got.ArchivePath != first {
		t.Fatalf("load result: %+v", got)
	}
	_, snapshot, err := openLayout(ctx, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.Descriptor, root.Descriptor) {
		t.Fatal("root changed")
	}
	third := filepath.Join(t.TempDir(), "third.tar")
	if _, err := svc.Save(ctx, loaded, third); err != nil {
		t.Fatal(err)
	}
	c, _ := os.ReadFile(third)
	if !bytes.Equal(a, c) {
		t.Fatal("round trip changed graph bytes")
	}
	if _, err := os.Stat(filepath.Join(layout, "secret")); err != nil {
		t.Fatal("source changed")
	}
}

type archiveTestEntry struct {
	name string
	kind byte
	body string
}

func archiveTestTar(t *testing.T, entries []archiveTestEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, entry := range entries {
		h := &tar.Header{Name: entry.name, Typeflag: entry.kind, Mode: 0644, Size: int64(len(entry.body))}
		if entry.kind == tar.TypeSymlink || entry.kind == tar.TypeLink {
			h.Linkname = "/tmp/escape"
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestArchiveRejectsHostileAndIncompleteInputs(t *testing.T) {
	goodLayout := archiveTestEntry{"oci-layout", tar.TypeReg, `{"imageLayoutVersion":"1.0.0"}`}
	cases := map[string][]byte{}
	for _, name := range []string{"../escape", "/escape", "blobs\\sha256\\escape", "blobs/../oci-layout"} {
		cases[name] = archiveTestTar(t, []archiveTestEntry{{name, tar.TypeReg, "bad"}})
	}
	for _, kind := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeFifo, tar.TypeChar} {
		cases[string(kind)] = archiveTestTar(t, []archiveTestEntry{{"oci-layout", kind, ""}})
	}
	cases["duplicate"] = archiveTestTar(t, []archiveTestEntry{goodLayout, goodLayout})
	cases["wrong-layout"] = archiveTestTar(t, []archiveTestEntry{{"oci-layout", tar.TypeReg, `{"imageLayoutVersion":"2.0.0"}`}})
	cases["missing-root"] = archiveTestTar(t, []archiveTestEntry{goodLayout, {"index.json", tar.TypeReg, `{"schemaVersion":2,"manifests":[]}`}})
	cases["corrupt-blob"] = archiveTestTar(t, []archiveTestEntry{goodLayout, {"blobs/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", tar.TypeReg, "corrupt"}})
	full := archiveTestTar(t, []archiveTestEntry{goodLayout})
	cases["missing-end"] = full[:len(full)-1024]
	cases["truncated-body"] = full[:514]
	cases["hidden-trailer"] = append(append([]byte{}, full...), []byte("hidden")...)
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "input.tar")
			output := filepath.Join(dir, "layout")
			if err := os.WriteFile(input, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := (&Service{}).Load(ctx, input, output); err == nil {
				t.Fatal("accepted invalid archive")
			}
			if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published output: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("temporary state leaked: %+v", entries)
			}
		})
	}
}

func TestArchiveNoOverwriteAndCancellation(t *testing.T) {
	src := memory.New()
	root := snapshotFor(t, src, []string{"example.test/image"}, []v1.Descriptor{object(t, src, "payload")})
	layout := layoutFor(t, src, root.Descriptor)
	svc := &Service{}
	dir := t.TempDir()
	archive := filepath.Join(dir, "bundle.tar")
	if _, err := svc.Save(ctx, layout, archive); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(archive)
	if _, err := svc.Save(ctx, layout, archive); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwrite error: %v", err)
	}
	link := filepath.Join(dir, "dangling")
	if err := os.Symlink("absent", link); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(ctx, layout, link); !errors.Is(err, ErrConflict) {
		t.Fatalf("symlink overwrite: %v", err)
	}
	if _, err := svc.Load(ctx, archive, link); !errors.Is(err, ErrConflict) {
		t.Fatalf("load symlink overwrite: %v", err)
	}
	if _, err := svc.Load(ctx, archive, layout); !errors.Is(err, ErrConflict) {
		t.Fatalf("layout overwrite: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, run := range []func() error{
		func() error { _, err := svc.Save(canceled, layout, filepath.Join(dir, "canceled.tar")); return err },
		func() error { _, err := svc.Load(canceled, archive, filepath.Join(dir, "canceled-layout")); return err },
	} {
		if err := run(); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error: %v", err)
		}
	}
	after, _ := os.ReadFile(archive)
	if !bytes.Equal(original, after) {
		t.Fatal("existing archive changed")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("temporary state leaked: %+v", entries)
	}
}

func TestArchiveMissingRequiredBlobAndFailureCleanup(t *testing.T) {
	src := memory.New()
	payload := object(t, src, "required payload")
	root := snapshotFor(t, src, []string{"example.test/image"}, []v1.Descriptor{payload})
	layout := layoutFor(t, src, root.Descriptor)
	svc := &Service{}
	dir := t.TempDir()
	archive := filepath.Join(dir, "bundle.tar")
	if _, err := svc.Save(ctx, layout, archive); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	var incomplete bytes.Buffer
	tr := tar.NewReader(bytes.NewReader(raw))
	tw := tar.NewWriter(&incomplete)
	removed := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "blobs/"+payload.Digest.Algorithm().String()+"/"+payload.Digest.Encoded() {
			removed = true
			continue
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			t.Fatal(err)
		}
	}
	if !removed {
		t.Fatal("did not remove required blob")
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.tar")
	if err := os.WriteFile(broken, incomplete.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	t.Setenv("TMPDIR", staging)
	output := filepath.Join(dir, "loaded")
	if _, err := svc.Load(ctx, broken, output); err == nil {
		t.Fatal("loaded missing required blob")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published incomplete layout: %v", err)
	}
	if entries, err := os.ReadDir(staging); err != nil || len(entries) != 0 {
		t.Fatalf("load temporary state leaked: %v %+v", err, entries)
	}
	// An invalid source must not leave an archive or sibling temporary directory.
	if err := os.Remove(filepath.Join(layout, "blobs", payload.Digest.Algorithm().String(), payload.Digest.Encoded())); err != nil {
		t.Fatal(err)
	}
	saveParent := t.TempDir()
	if _, err := svc.Save(ctx, layout, filepath.Join(saveParent, "bad.tar")); err == nil {
		t.Fatal("saved incomplete source")
	}
	if entries, err := os.ReadDir(saveParent); err != nil || len(entries) != 0 {
		t.Fatalf("save temporary state leaked: %v %+v", err, entries)
	}
}

func TestArchiveRejectsInvalidNestedBundleMetadata(t *testing.T) {
	store := memory.New()
	inner := snapshotFor(t, store, []string{"example.test/member"}, []v1.Descriptor{object(t, store, "member")})
	raw, err := content.FetchAll(ctx, store, inner.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	index.Annotations[bundle.MetadataDigestKey] = digest.FromString("not selected metadata").String()
	raw, err = json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	invalid := put(t, store, v1.MediaTypeImageIndex, raw)
	invalid.ArtifactType = bundle.BundleArtifactType
	outer := snapshotFor(t, store, []string{"example.test/inner"}, []v1.Descriptor{invalid})
	// A full hash-valid closure can still contain false nested Bale metadata.
	if err := bundle.VerifyClosure(ctx, store, outer.Descriptor); err != nil {
		t.Fatalf("fixture closure: %v", err)
	}
	layout := layoutFor(t, store, outer.Descriptor)
	archive := filepath.Join(t.TempDir(), "invalid.tar")
	fp, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLayoutTar(ctx, fp, layout); err != nil {
		t.Fatal(err)
	}
	if err := fp.Close(); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "layout")
	if _, err := (&Service{}).Load(ctx, archive, output); err == nil {
		t.Fatal("loaded invalid nested metadata")
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published invalid layout: %v", err)
	}
	saved := filepath.Join(t.TempDir(), "saved.tar")
	if _, err := (&Service{}).Save(ctx, layout, saved); err == nil {
		t.Fatal("saved invalid nested metadata")
	}
	if _, err := os.Lstat(saved); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published invalid archive: %v", err)
	}
}

func TestArchiveAcceptsFilesystemTarPaths(t *testing.T) {
	store := memory.New()
	snapshot := snapshotFor(t, store, []string{"example.test/image"}, []v1.Descriptor{object(t, store, "payload")})
	layout := layoutFor(t, store, snapshot.Descriptor)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(layout, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == layout {
			return nil
		}
		rel, err := filepath.Rel(layout, name)
		if err != nil {
			return err
		}
		h := &tar.Header{Name: "./" + filepath.ToSlash(rel), Typeflag: tar.TypeDir, Mode: 0755}
		if entry.IsDir() {
			h.Name += "/"
			return tw.WriteHeader(h)
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		h.Typeflag = tar.TypeReg
		h.Mode = 0644
		h.Size = int64(len(raw))
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err = tw.Write(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "filesystem.tar")
	if err := os.WriteFile(input, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "loaded")
	got, err := (&Service{}).Load(ctx, input, output)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Descriptor, snapshot.Descriptor) {
		t.Fatal("root changed")
	}
	// Prefix aliases must not permit two competing copies of the same member.
	duplicate := archiveTestTar(t, []archiveTestEntry{
		{"./oci-layout", tar.TypeReg, `{"imageLayoutVersion":"1.0.0"}`},
		{"oci-layout", tar.TypeReg, `{"imageLayoutVersion":"1.0.0"}`},
	})
	if err := os.WriteFile(input, duplicate, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Service{}).Load(ctx, input, filepath.Join(t.TempDir(), "duplicate")); err == nil {
		t.Fatal("accepted canonical path alias duplicate")
	}
}
