package store_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/strio-dev/bale/store"
)

func TestManagedArchiveStreamsHydrateOnceAndRoundTrip(t *testing.T) {
	f, server, client, recipe := source(t)
	defer server.Close()
	s := local(t, client)
	created, err := s.Create(t.Context(), recipe, store.CreateOptions{Tags: []string{"release:v1"}})
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	saved, err := s.SaveTo(t.Context(), "release:v1", &archive)
	if err != nil || saved.Descriptor.Digest != created.Descriptor.Digest || saved.LayoutPath != "" || saved.ArchivePath != "" {
		t.Fatalf("save stream: %+v %v", saved, err)
	}
	if f.gets.Load() != 1 {
		t.Fatalf("payload fetch count: %d", f.gets.Load())
	}
	server.Close()
	path := filepath.Join(t.TempDir(), "bundle.tar")
	if _, err := s.Save(t.Context(), "release:v1", path); err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(file, archive.Bytes()) {
		t.Fatalf("file and stream archives differ: %v", err)
	}
	destination := local(t, client)
	loaded, err := destination.LoadFrom(t.Context(), bytes.NewReader(archive.Bytes()), []string{"imported:v1"})
	if err != nil || loaded.Descriptor.Digest != created.Descriptor.Digest || loaded.LayoutPath != "" || loaded.ArchivePath != "" {
		t.Fatalf("load stream: %+v %v", loaded, err)
	}
	inspected, err := destination.Inspect(t.Context(), "imported:v1")
	if err != nil || inspected.Descriptor.Digest != created.Descriptor.Digest || inspected.MetadataOnly {
		t.Fatalf("imported stream: %+v %v", inspected, err)
	}
}

type failedArchiveReader struct{}

var errArchiveRead = errors.New("archive input failed")

func (failedArchiveReader) Read([]byte) (int, error) { return 0, errArchiveRead }

func TestManagedFailedArchiveStreamPublishesNothing(t *testing.T) {
	_, server, client, _ := source(t)
	defer server.Close()
	s := local(t, client)
	result, err := s.LoadFrom(t.Context(), failedArchiveReader{}, []string{"must-not-exist"})
	if !errors.Is(err, errArchiveRead) || result.LayoutPath != "" || result.ArchivePath != "" {
		t.Fatalf("failed stream: %+v %v", result, err)
	}
	entries, err := s.List(t.Context())
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed stream retained a root: %+v %v", entries, err)
	}
	paths, err := filepath.Glob(filepath.Join(s.Path(), ".load-*"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("failed stream leaked staging: %v %v", paths, err)
	}
}
