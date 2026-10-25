package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/content/oci"
)

type captureRaceSource struct {
	content.ReadOnlyStorage
	descriptor v1.Descriptor
	reader     io.ReadCloser
	insert     func() error
}

func (s captureRaceSource) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	if d.Digest != s.descriptor.Digest {
		return s.ReadOnlyStorage.Fetch(ctx, d)
	}
	// Fetch is called after the destination's negative Exists check. Wait for a
	// second writer to publish the same object before Push sees this reader.
	done := make(chan error, 1)
	go func() { done <- s.insert() }()
	if err := <-done; err != nil {
		return nil, err
	}
	return s.reader, nil
}

type captureRaceReader struct {
	io.Reader
	closed bool
	err    error
}

func (r *captureRaceReader) Close() error {
	r.closed = true
	return r.err
}

func TestCaptureRecorderHandlesConcurrentInsertion(t *testing.T) {
	raw := []byte("verified capture metadata")
	d := content.NewDescriptorFromBytes("application/example", raw)
	closeErr := errors.New("source close failed")
	for _, scenario := range []string{"valid", "corrupt source", "truncated source", "trailing source", "corrupt destination", "canceled", "close failure"} {
		t.Run(scenario, func(t *testing.T) {
			testCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sinkRoot := t.TempDir()
			sink, err := oci.NewStorage(sinkRoot)
			if err != nil {
				t.Fatal(err)
			}
			input := append([]byte(nil), raw...)
			var wantErr error
			switch scenario {
			case "corrupt source", "corrupt destination":
				wantErr = content.ErrMismatchedDigest
				if scenario == "corrupt source" {
					input[0] ^= 1
				}
			case "truncated source":
				input = input[:len(input)-1]
				wantErr = io.ErrUnexpectedEOF
			case "trailing source":
				input = append(input, '!')
				wantErr = content.ErrTrailingData
			case "canceled":
				wantErr = context.Canceled
			case "close failure":
				wantErr = closeErr
			}
			reader := &captureRaceReader{Reader: bytes.NewReader(input)}
			if scenario == "close failure" {
				reader.err = closeErr
			}
			source := captureRaceSource{
				ReadOnlyStorage: memory.New(), descriptor: d, reader: reader,
				insert: func() error {
					if err := sink.Push(testCtx, d, bytes.NewReader(raw)); err != nil {
						return err
					}
					if scenario == "corrupt destination" {
						path := filepath.Join(sinkRoot, "blobs", d.Digest.Algorithm().String(), d.Digest.Encoded())
						if err := os.Chmod(path, 0600); err != nil {
							return err
						}
						return os.WriteFile(path, bytes.Repeat([]byte("!"), len(raw)), 0600)
					}
					if scenario == "canceled" {
						cancel()
					}
					return nil
				},
			}
			got, err := content.FetchAll(testCtx, captureRecorder{source: source, target: sink}, d)
			if !errors.Is(err, wantErr) {
				t.Fatalf("capture race: got %v, want %v", err, wantErr)
			}
			if wantErr == nil && !bytes.Equal(got, raw) {
				t.Fatalf("captured bytes changed: %q", got)
			}
			if !reader.closed {
				t.Fatal("source reader was not closed")
			}
		})
	}
}

func TestCopyGraphHandlesConcurrentBlobInsertion(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "corrupt source"}[corrupt], func(t *testing.T) {
			sourceStore := memory.New()
			raw := []byte("racing graph payload")
			root := object(t, sourceStore, string(raw))
			d := content.NewDescriptorFromBytes("application/example", raw)
			sink, err := oci.NewStorage(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			input := append([]byte(nil), raw...)
			if corrupt {
				input[0] ^= 1
			}
			reader := &captureRaceReader{Reader: bytes.NewReader(input)}
			source := captureRaceSource{
				ReadOnlyStorage: sourceStore, descriptor: d, reader: reader,
				insert: func() error { return sink.Push(t.Context(), d, bytes.NewReader(raw)) },
			}
			stats := &Stats{}
			err = copyGraph(t.Context(), source, sink, root, stats, nil)
			if corrupt {
				if !errors.Is(err, content.ErrMismatchedDigest) {
					t.Fatalf("corrupt racing source: %v", err)
				}
				if exists, err := sink.Exists(t.Context(), root); err != nil || exists {
					t.Fatalf("invalid graph published root: %t %v", exists, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := bundle.VerifyClosure(t.Context(), sink, root); err != nil {
					t.Fatal(err)
				}
				if stats.ObjectsSkipped != 1 || stats.BlobBytesUploaded != 2 {
					t.Fatalf("raced object counted as uploaded: %+v", stats)
				}
			}
			if !reader.closed {
				t.Fatal("source reader was not closed")
			}
		})
	}
}
