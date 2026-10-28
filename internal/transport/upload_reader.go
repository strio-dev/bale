package transport

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
)

// uploadReader hands the source back from an uploader that may keep reading
// after Push returns. Only its owner verifies and closes the source.
type uploadReader struct {
	mu       sync.Mutex
	stopped  atomic.Bool
	read     atomic.Int64
	size     int64
	verified *content.VerifyReader
	source   io.ReadCloser
	cancel   context.CancelFunc
}

func openUploadReader(ctx context.Context, d v1.Descriptor, fetch func(context.Context) (io.ReadCloser, error)) (*uploadReader, error) {
	fetchCtx, cancel := context.WithCancel(ctx)
	source, err := fetch(fetchCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	return &uploadReader{
		size:     d.Size,
		verified: content.NewVerifyReader(source, d),
		source:   source,
		cancel:   cancel,
	}, nil
}

func (r *uploadReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped.Load() {
		return 0, io.EOF
	}
	n, err := r.verified.Read(p)
	r.read.Add(int64(n))
	return n, err
}

func (r *uploadReader) finish(ctx context.Context, uploadErr error, alreadyExists bool) (retErr error) {
	// A returned HTTP response is not a request-writer completion signal.
	// Seal future reads, then wait for the current Read before touching either
	// the verifier or the source. Cancel an unfinished fetch to unblock its
	// HTTP read; closing the raw source concurrently is not generally safe.
	r.stopped.Store(true)
	if !r.mu.TryLock() {
		if r.read.Load() < r.size {
			r.cancel()
		}
		r.mu.Lock()
	}
	defer r.mu.Unlock()
	defer func() {
		r.cancel()
		retErr = errors.Join(retErr, r.source.Close())
	}()
	if uploadErr != nil {
		if !alreadyExists {
			return uploadErr
		}
		// Another CAS writer may win without the destination reading our input.
		// With ownership restored, consume and verify the remaining source.
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, r.verified); err != nil {
			return err
		}
	}
	return errors.Join(r.verified.Verify(), ctx.Err())
}
