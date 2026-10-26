package transport

import (
	"context"
	"io"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
)

// RepositoryFactory opens a caller-owned backend for a canonical
// registry/repository name, without a tag or digest. The caller retains ownership
// of the backend, its lifetime, authentication, certificates and network policy.
type RepositoryFactory func(context.Context, string) (RegistryRepository, error)

// RegistryRepository provides manifest operations and a separate blob namespace.
// Push publishes bytes; Bale maintains and verifies fallback referrer indexes.
// Implement registry.TagLister as well to support all-tags selection.
type RegistryRepository interface {
	oras.Target
	Blobs() RegistryBlobStore

	// ReferrerPages enumerates raw native Referrers API indexes for all artifact
	// types. The backend handles authenticated requests and pagination; Bale
	// validates the pages and their referenced envelopes. Invoke fn synchronously
	// and stop on its first error. The backend closes each page's stream.
	//
	// Return errdef.ErrUnsupported to explicitly provide fallback-only discovery.
	// Such a backend cannot discover native-only referrers. Authentication,
	// cancellation, malformed responses and other operational errors must retain
	// their error identity rather than being converted to ErrUnsupported.
	ReferrerPages(context.Context, v1.Descriptor, func(ReferrerPage) error) error
}

// RegistryBlobStore accesses blobs independently of their media type. An OCI
// envelope can be a blob layer as well as a manifest. Resolve must report the
// actual digest and size; callers must not infer them from the supplied digest.
type RegistryBlobStore interface {
	content.Storage
	content.Resolver
}

// ReferrerPage contains one native Referrers API response. MediaType is required;
// Digest is empty when unadvertised and Size is -1 when unknown. Content is valid
// only during the enumeration callback and remains owned by the backend.
type ReferrerPage struct {
	MediaType string
	Digest    digest.Digest
	Size      int64
	Content   io.Reader
}
