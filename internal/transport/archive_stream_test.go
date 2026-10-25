package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/memory"
)

func TestArchiveStreamsMatchFileArchive(t *testing.T) {
	src := memory.New()
	root := snapshotFor(t, src, []string{"example.test/image"}, []v1.Descriptor{object(t, src, "stream payload")})
	layout := layoutFor(t, src, root.Descriptor)
	svc := &Service{}
	archive := filepath.Join(t.TempDir(), "bundle.tar")
	if _, err := svc.Save(ctx, layout, archive); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	saved, err := svc.SaveTo(ctx, layout, &stream)
	if err != nil || !reflect.DeepEqual(saved.Descriptor, root.Descriptor) || saved.ArchivePath != "" || saved.LayoutPath != layout {
		t.Fatalf("save stream: %+v %v", saved, err)
	}
	if !bytes.Equal(want, stream.Bytes()) {
		t.Fatal("stream and file archives differ")
	}
	output := filepath.Join(t.TempDir(), "restored")
	loaded, err := svc.LoadFrom(ctx, bytes.NewReader(stream.Bytes()), output)
	if err != nil || !reflect.DeepEqual(loaded.Descriptor, root.Descriptor) || loaded.ArchivePath != "" || loaded.LayoutPath != output {
		t.Fatalf("load stream: %+v %v", loaded, err)
	}
	var again bytes.Buffer
	if _, err := svc.SaveTo(ctx, output, &again); err != nil || !bytes.Equal(want, again.Bytes()) {
		t.Fatalf("stream round trip changed archive: %v", err)
	}
}

var errArchiveStream = errors.New("archive stream failed")

type failingArchiveStream struct{}

func (failingArchiveStream) Read([]byte) (int, error)  { return 0, errArchiveStream }
func (failingArchiveStream) Write([]byte) (int, error) { return 0, errArchiveStream }

type archiveReadFunc func([]byte) (int, error)

func (f archiveReadFunc) Read(p []byte) (int, error) { return f(p) }

type archiveWriteFunc func([]byte) (int, error)

func (f archiveWriteFunc) Write(p []byte) (int, error) { return f(p) }

func TestArchiveStreamErrorsAndCancellation(t *testing.T) {
	src := memory.New()
	root := snapshotFor(t, src, []string{"example.test/image"}, []v1.Descriptor{object(t, src, "stream payload")})
	layout := layoutFor(t, src, root.Descriptor)
	svc := &Service{}
	if _, err := svc.SaveTo(ctx, layout, failingArchiveStream{}); !errors.Is(err, errArchiveStream) {
		t.Fatalf("write failure: %v", err)
	}
	for name, input := range map[string]io.Reader{"reader-error": failingArchiveStream{}, "corrupt": bytes.NewBufferString("invalid tar")} {
		t.Run(name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "restored")
			if _, err := svc.LoadFrom(ctx, input, output); err == nil {
				t.Fatal("load accepted failed stream")
			} else if name == "reader-error" && !errors.Is(err, errArchiveStream) {
				t.Fatalf("read failure: %v", err)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("failed stream published a layout: %v", err)
			}
		})
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := svc.SaveTo(canceled, layout, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("save cancellation: %v", err)
	}
	if _, err := svc.LoadFrom(canceled, bytes.NewReader(nil), filepath.Join(t.TempDir(), "restored")); !errors.Is(err, context.Canceled) {
		t.Fatalf("load cancellation: %v", err)
	}
}

func TestArchiveStreamCancellationDuringIO(t *testing.T) {
	src := memory.New()
	root := snapshotFor(t, src, []string{"example.test/image"}, []v1.Descriptor{object(t, src, "stream payload")})
	layout := layoutFor(t, src, root.Descriptor)
	svc := &Service{}
	var raw bytes.Buffer
	if _, err := svc.SaveTo(ctx, layout, &raw); err != nil {
		t.Fatal(err)
	}
	loadContext, cancelLoad := context.WithCancel(ctx)
	defer cancelLoad()
	in := bytes.NewReader(raw.Bytes())
	reader := archiveReadFunc(func(p []byte) (int, error) {
		n, err := in.Read(p)
		cancelLoad()
		return n, err
	})
	output := filepath.Join(t.TempDir(), "restored")
	if _, err := svc.LoadFrom(loadContext, reader, output); !errors.Is(err, context.Canceled) {
		t.Fatalf("load cancellation during read: %v", err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("canceled read published layout: %v", err)
	}
	saveContext, cancelSave := context.WithCancel(ctx)
	defer cancelSave()
	writer := archiveWriteFunc(func(p []byte) (int, error) {
		cancelSave()
		return len(p), nil
	})
	if _, err := svc.SaveTo(saveContext, layout, writer); !errors.Is(err, context.Canceled) {
		t.Fatalf("save cancellation during write: %v", err)
	}
}
