package transport

import (
	"context"
	"io"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/memory"
)

// ORAS memory.Store indexes manifests while pushing. This adapter uses its
// verified CAS without interpreting payloads that happen to declare a known
// envelope media type, and keys content independently of descriptive media type.
type metadataStore struct{ store *memory.Store }

func newMetadataStore() *metadataStore { return &metadataStore{store: memory.New()} }
func cacheDescriptor(d v1.Descriptor) v1.Descriptor {
	d.MediaType = "application/octet-stream"
	return d
}
func (s *metadataStore) Push(ctx context.Context, d v1.Descriptor, r io.Reader) error {
	return s.store.Push(ctx, cacheDescriptor(d), r)
}
func (s *metadataStore) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	return s.store.Fetch(ctx, cacheDescriptor(d))
}
func (s *metadataStore) Exists(ctx context.Context, d v1.Descriptor) (bool, error) {
	return s.store.Exists(ctx, cacheDescriptor(d))
}
