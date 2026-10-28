package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
)

const copyUploadPayload = "payload checked after its HTTP response"

type copyUploadReader struct {
	*bytes.Reader
	closed     atomic.Bool
	closeCalls atomic.Int32
	closeErr   error
	readErr    error
	eofOnce    sync.Once
	onEOF      func()
}

func (r *copyUploadReader) Read(p []byte) (int, error) {
	if r.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if r.readErr != nil {
		return 0, r.readErr
	}
	n, err := r.Reader.Read(p)
	if err == io.EOF && r.onEOF != nil {
		r.eofOnce.Do(r.onEOF)
	}
	return n, err
}

func (r *copyUploadReader) Close() error {
	r.closeCalls.Add(1)
	r.closed.Store(true)
	return r.closeErr
}

type copyUploadFetcher struct {
	content.Fetcher
	target digest.Digest
	reader func(context.Context) io.ReadCloser
}

func (f copyUploadFetcher) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	if d.Digest == f.target {
		return f.reader(ctx), nil
	}
	return f.Fetcher.Fetch(ctx, d)
}

type copyUploadStore struct {
	*memory.Store
	target digest.Digest
	upload func(io.Reader) error
}

func (s *copyUploadStore) Push(ctx context.Context, d v1.Descriptor, r io.Reader) error {
	if d.Digest == s.target {
		return s.upload(r)
	}
	return s.Store.Push(ctx, d, r)
}

type copyUploadRegistry struct {
	*memory.Store
	blobs *copyUploadStore
}

func (r *copyUploadRegistry) Blobs() RegistryBlobStore { return r.blobs }

func (*copyUploadRegistry) ReferrerPages(context.Context, v1.Descriptor, func(ReferrerPage) error) error {
	return errdef.ErrUnsupported
}

func (r *copyUploadRegistry) Mount(_ context.Context, _ v1.Descriptor, _ string, fetch func() (io.ReadCloser, error)) (retErr error) {
	rc, err := fetch()
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, rc.Close()) }()
	return r.blobs.upload(rc)
}

func copyUploadFixture(t *testing.T, reader io.ReadCloser) (copyUploadFetcher, v1.Descriptor, v1.Descriptor) {
	t.Helper()
	store := memory.New()
	root := object(t, store, copyUploadPayload)
	payload := content.NewDescriptorFromBytes("application/example", []byte(copyUploadPayload))
	return copyUploadFetcher{Fetcher: store, target: payload.Digest, reader: func(context.Context) io.ReadCloser { return reader }}, root, payload
}

func copyUploadDestination(mount bool, payload v1.Descriptor, upload func(io.Reader) error) (content.Storage, map[digest.Digest]string) {
	store := &copyUploadStore{Store: memory.New(), target: payload.Digest, upload: upload}
	if !mount {
		return store, nil
	}
	return &registryRepository{
		Reference: registry.Reference{Registry: "registry.example", Repository: "destination"},
		backend:   &copyUploadRegistry{Store: memory.New(), blobs: store},
	}, map[digest.Digest]string{payload.Digest: "registry.example/source"}
}

// A RoundTripper may return a response while its request-body goroutine is
// still doing the final EOF read. The source EOF read is also Verify's trailing
// data check: these channels force the two activities to meet without sleeps.
func TestCopyUploadVerificationOwnsReaderAfterHTTPResponse(t *testing.T) {
	for _, mount := range []bool{false, true} {
		t.Run(map[bool]string{false: "push", true: "mount fallback"}[mount], func(t *testing.T) {
			testCtx, cancel := context.WithCancel(t.Context())
			verifyStarted := make(chan struct{})
			finishVerify := make(chan struct{})
			reader := &copyUploadReader{Reader: bytes.NewReader([]byte(copyUploadPayload)), onEOF: func() {
				close(verifyStarted)
				select {
				case <-finishVerify:
				case <-testCtx.Done():
				}
			}}
			source, root, payload := copyUploadFixture(t, reader)
			bodyDone := make(chan error, 1)
			bodyExited := make(chan struct{})
			var bodyStarted atomic.Bool
			t.Cleanup(func() {
				cancel()
				if bodyStarted.Load() {
					<-bodyExited
				}
			})
			client := &http.Client{Transport: deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
				if _, err := io.CopyN(io.Discard, req.Body, req.ContentLength); err != nil {
					return nil, err
				}
				bodyStarted.Store(true)
				go func() {
					defer close(bodyExited)
					select {
					case <-verifyStarted:
					case <-testCtx.Done():
						bodyDone <- testCtx.Err()
						return
					}
					close(finishVerify)
					var extra [1]byte
					n, readErr := req.Body.Read(extra[:])
					closeErr := req.Body.Close()
					if n != 0 || readErr == nil {
						bodyDone <- fmt.Errorf("final HTTP body read returned %d bytes, %v", n, readErr)
						return
					}
					bodyDone <- closeErr
				}()
				return &http.Response{StatusCode: http.StatusCreated, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
			})}
			destination, mounts := copyUploadDestination(mount, payload, func(body io.Reader) error {
				req, err := http.NewRequestWithContext(testCtx, http.MethodPut, "https://registry.example/upload", body)
				if err != nil {
					return err
				}
				req.ContentLength = payload.Size
				response, err := client.Do(req)
				if err != nil {
					return err
				}
				return response.Body.Close()
			})
			if err := copyGraph(testCtx, source, destination, root, &Stats{}, mounts); err != nil {
				t.Fatal(err)
			}
			if err := <-bodyDone; err != nil {
				t.Fatal(err)
			}
			if got := reader.closeCalls.Load(); got != 1 {
				t.Fatalf("source closed %d times, want 1", got)
			}
		})
	}
}

func TestCopyUploadPreservesIntegrityAndErrors(t *testing.T) {
	uploadErr := errors.New("upload failed before consuming body")
	closeErr := errors.New("source close failed")
	readErr := errors.New("source read failed")
	for _, mount := range []bool{false, true} {
		t.Run(map[bool]string{false: "push", true: "mount fallback"}[mount], func(t *testing.T) {
			for _, scenario := range []string{"valid", "early success", "upload error", "read error", "close error", "upload and close errors", "truncated", "digest mismatch", "trailing data", "already exists", "already exists truncated", "already exists digest mismatch", "already exists trailing data"} {
				t.Run(scenario, func(t *testing.T) {
					input := []byte(copyUploadPayload)
					reader := &copyUploadReader{}
					var wantErr, responseErr error
					switch scenario {
					case "upload error", "upload and close errors":
						responseErr, wantErr = uploadErr, uploadErr
					case "read error":
						reader.readErr, wantErr = readErr, readErr
					case "truncated", "already exists truncated":
						input, wantErr = input[:len(input)-1], io.ErrUnexpectedEOF
					case "digest mismatch", "already exists digest mismatch":
						input[0] ^= 1
						wantErr = content.ErrMismatchedDigest
					case "trailing data", "already exists trailing data":
						input, wantErr = append(input, '!'), content.ErrTrailingData
					}
					if strings.HasPrefix(scenario, "already exists") {
						responseErr = errdef.ErrAlreadyExists
						if mount {
							wantErr = responseErr
						}
					}
					if scenario == "close error" || scenario == "upload and close errors" {
						reader.closeErr = closeErr
						if wantErr == nil {
							wantErr = closeErr
						}
					}
					reader.Reader = bytes.NewReader(input)
					source, root, payload := copyUploadFixture(t, reader)
					destination, mounts := copyUploadDestination(mount, payload, func(body io.Reader) error {
						if responseErr != nil || scenario == "early success" {
							return responseErr
						}
						_, err := io.CopyN(io.Discard, body, payload.Size)
						return err
					})
					err := copyGraph(t.Context(), source, destination, root, &Stats{}, mounts)
					if scenario == "early success" {
						if err == nil {
							t.Fatal("accepted successful upload without consuming its payload")
						}
					} else if !errors.Is(err, wantErr) {
						t.Fatalf("got %v, want %v", err, wantErr)
					}
					if reader.closeErr != nil && !errors.Is(err, reader.closeErr) {
						t.Fatalf("lost source close failure: %v", err)
					}
					if got := reader.closeCalls.Load(); got != 1 {
						t.Fatalf("source closed %d times, want 1", got)
					}
				})
			}
		})
	}
}

// Cancellation releases a source read without requiring Close to race it. The
// second request-body read happens after cleanup and must not touch the source.
type copyUploadBlockedReader struct {
	ctx               context.Context
	started           chan struct{}
	finished          chan struct{}
	closed            chan struct{}
	closeOnce         sync.Once
	active            atomic.Bool
	closeWhileReading atomic.Bool
	readCalls         atomic.Int32
	closeCalls        atomic.Int32
}

func (r *copyUploadBlockedReader) Read([]byte) (int, error) {
	if r.readCalls.Add(1) != 1 {
		return 0, io.ErrClosedPipe
	}
	r.active.Store(true)
	defer func() {
		r.active.Store(false)
		close(r.finished)
	}()
	close(r.started)
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-r.closed:
		return 0, io.ErrClosedPipe
	}
}

func (r *copyUploadBlockedReader) Close() error {
	r.closeCalls.Add(1)
	if r.active.Load() {
		r.closeWhileReading.Store(true)
	}
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

func TestCopyUploadEarlyResponseWaitsForActiveReader(t *testing.T) {
	uploadErr := errors.New("registry rejected upload before reading its body")
	for _, mount := range []bool{false, true} {
		t.Run(map[bool]string{false: "push", true: "mount fallback"}[mount], func(t *testing.T) {
			for _, scenario := range []string{"success", "error", "canceled"} {
				t.Run(scenario, func(t *testing.T) {
					testCtx, cancel := context.WithCancel(t.Context())
					reader := &copyUploadBlockedReader{started: make(chan struct{}), finished: make(chan struct{}), closed: make(chan struct{})}
					source, root, payload := copyUploadFixture(t, nil)
					source.reader = func(ctx context.Context) io.ReadCloser {
						reader.ctx = ctx
						return reader
					}
					bodyDone := make(chan error, 1)
					bodyExited := make(chan struct{})
					var bodyStarted atomic.Bool
					t.Cleanup(func() {
						cancel()
						reader.closeOnce.Do(func() { close(reader.closed) })
						if bodyStarted.Load() {
							<-bodyExited
						}
					})
					client := &http.Client{Transport: deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
						bodyStarted.Store(true)
						go func() {
							defer close(bodyExited)
							var body [1]byte
							_, _ = req.Body.Read(body[:])
							<-reader.closed
							n, err := req.Body.Read(body[:])
							closeErr := req.Body.Close()
							if n != 0 || err == nil {
								bodyDone <- fmt.Errorf("body read after cleanup returned %d bytes, %v", n, err)
								return
							}
							bodyDone <- closeErr
						}()
						<-reader.started
						if scenario == "canceled" {
							cancel()
						}
						status := http.StatusCreated
						if scenario == "error" {
							status = http.StatusBadRequest
						}
						return &http.Response{StatusCode: status, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
					})}
					destination, mounts := copyUploadDestination(mount, payload, func(body io.Reader) error {
						req, err := http.NewRequestWithContext(testCtx, http.MethodPut, "https://registry.example/upload", body)
						if err != nil {
							return err
						}
						req.ContentLength = payload.Size
						response, err := client.Do(req)
						if err != nil {
							return err
						}
						if err := response.Body.Close(); err != nil {
							return err
						}
						if response.StatusCode != http.StatusCreated {
							return uploadErr
						}
						return nil
					})
					err := copyGraph(testCtx, source, destination, root, &Stats{}, mounts)
					if err == nil {
						t.Fatal("accepted upload whose request body was canceled before consumption")
					}
					if scenario == "error" && !errors.Is(err, uploadErr) {
						t.Fatalf("lost upload error: %v", err)
					}
					if scenario == "canceled" && !errors.Is(err, context.Canceled) {
						t.Fatalf("lost cancellation: %v", err)
					}
					if err := <-bodyDone; err != nil {
						t.Fatal(err)
					}
					if reader.closeWhileReading.Load() {
						t.Fatal("closed source while its upload read was still active")
					}
					if got := reader.readCalls.Load(); got != 1 {
						t.Fatalf("late HTTP body read reached source: %d source reads, want 1", got)
					}
					if got := reader.closeCalls.Load(); got != 1 {
						t.Fatalf("source closed %d times, want 1", got)
					}
				})
			}
		})
	}
}
