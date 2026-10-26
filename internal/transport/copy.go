package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
)

// copyGraph walks children even when a destination already has its parent.
// Registry existence alone is not evidence of a complete graph.
func copyGraph(ctx context.Context, src content.Fetcher, dst content.Storage, root v1.Descriptor, stats *Stats, mountSource map[digest.Digest]string) error {
	blobs, registryDestination := registryBlobs(dst)
	seen := map[string]bool{}
	var walk func(v1.Descriptor, bool) error
	walk = func(d v1.Descriptor, isEnvelope bool) (retErr error) {
		key := visitKey(d, isEnvelope)
		if seen[key] {
			return nil
		}
		seen[key] = true
		var raw []byte
		if isEnvelope {
			var err error
			raw, err = fetchEnvelopeBytes(ctx, src, d)
			if err != nil {
				return err
			}
			e, err := bundle.ParseEnvelope(d, raw)
			if err != nil {
				return fmt.Errorf("%w: %w", ErrIntegrity, err)
			}
			for i, child := range e.Children {
				if err := walk(child, bundle.IsEnvelopeChild(d, i, child)); err != nil {
					return err
				}
				// Some registries validate layers through their blob service,
				// independently of an existing manifest registration. Preserve both
				// views before publishing (or reusing) the containing manifest.
				if (d.MediaType == v1.MediaTypeImageManifest || d.MediaType == bundle.DockerManifest) && i > 0 && bundle.IsEnvelope(child.MediaType) {
					if registryDestination {
						if err := registerEnvelopeLayerBlob(ctx, src, dst, child, stats); err != nil {
							return err
						}
					}
				}
			}
		}
		destination := dst
		var exists bool
		var err error
		if registryDestination && !isEnvelope {
			destination = blobs
			exists, err = remoteBlobExists(ctx, dst, d)
		} else {
			exists, err = destination.Exists(ctx, d)
		}
		if err != nil {
			return err
		}
		if exists {
			stats.ObjectsSkipped++
			return nil
		}
		if isEnvelope {
			if err := destination.Push(ctx, d, bytes.NewReader(raw)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
				return err
			}
			stats.ManifestBytesUploaded += int64(len(raw))
			return nil
		}
		fetch := func() (io.ReadCloser, error) {
			if d.Data != nil {
				return io.NopCloser(bytes.NewReader(d.Data)), nil
			}
			if blobs, ok := registryBlobs(src); ok {
				return blobs.Fetch(ctx, d)
			}
			return src.Fetch(ctx, d)
		}
		if mounter, destinationRef, ok := registryMount(dst); ok && mountSource != nil {
			if name := mountSource[d.Digest]; name != "" {
				ref, err := registry.ParseReference(name)
				if err != nil {
					return err
				}
				if ref.Registry == destinationRef.Registry && ref.Repository != destinationRef.Repository {
					uploaded := false
					var verified *content.VerifyReader
					var mountedReader io.ReadCloser
					err = mounter.Mount(ctx, d, ref.Repository, func() (io.ReadCloser, error) {
						uploaded = true
						rc, err := fetch()
						if err != nil {
							return nil, err
						}
						mountedReader = rc
						verified = content.NewVerifyReader(rc, d)
						return io.NopCloser(verified), nil
					})
					if mountedReader != nil {
						defer func() { retErr = errors.Join(retErr, mountedReader.Close()) }()
					}
					if err != nil {
						return err
					}
					if uploaded {
						if err := verified.Verify(); err != nil {
							return err
						}
						stats.BlobBytesUploaded += d.Size
					} else {
						if exists, err := remoteBlobExists(ctx, dst, d); err != nil {
							return err
						} else if !exists {
							return fmt.Errorf("mounted blob %s is unavailable", d.Digest)
						}
						stats.BlobsMounted++
					}
					return nil
				}
			}
		}
		rc, err := fetch()
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, rc.Close()) }()
		verified := content.NewVerifyReader(rc, d)
		pushErr := destination.Push(ctx, d, verified)
		alreadyExists := errors.Is(pushErr, errdef.ErrAlreadyExists)
		if pushErr != nil {
			if !alreadyExists {
				return pushErr
			}
			// Push may skip its reader when another writer won the same CAS
			// insertion. Consume the remaining source before verifying it.
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := io.Copy(io.Discard, verified); err != nil {
				return err
			}
		}
		if err := errors.Join(verified.Verify(), ctx.Err()); err != nil {
			return err
		}
		if alreadyExists {
			stats.ObjectsSkipped++
		} else {
			stats.BlobBytesUploaded += d.Size
		}
		return nil
	}
	if err := walk(root, true); err != nil {
		return fmt.Errorf("copy %s: %w", root.Digest, err)
	}
	return nil
}

func registerEnvelopeLayerBlob(ctx context.Context, src content.Fetcher, dst content.Storage, d v1.Descriptor, stats *Stats) error {
	exists, err := remoteBlobExists(ctx, dst, d)
	if err != nil {
		return err
	}
	if exists {
		stats.ObjectsSkipped++
		return nil
	}
	// Envelope content may exist only at the source manifest endpoint (or be
	// inline). Fetch and verify that exact content rather than treating its
	// additional destination blob registration as a source payload edge.
	raw, err := fetchEnvelopeBytes(ctx, src, d)
	if err != nil {
		return err
	}
	blobs, ok := registryBlobs(dst)
	if !ok {
		return fmt.Errorf("%w: destination cannot store layer blobs", ErrUnsupported)
	}
	if err := blobs.Push(ctx, d, bytes.NewReader(raw)); err != nil {
		if errors.Is(err, errdef.ErrAlreadyExists) {
			stats.ObjectsSkipped++
			return nil
		}
		return err
	}
	stats.BlobBytesUploaded += d.Size
	return nil
}

// verifyRemoteClosure verifies original envelope bytes and checks payload
// availability without downloading reused payloads. Full offline verification
// reads payload bytes in openLayout and Pull.
func verifyRemoteClosure(ctx context.Context, repo content.ReadOnlyStorage, root v1.Descriptor) error {
	seen := map[string]bool{}
	var walk func(v1.Descriptor, bool) error
	walk = func(d v1.Descriptor, isEnvelope bool) error {
		key := visitKey(d, isEnvelope)
		if seen[key] {
			return nil
		}
		seen[key] = true
		if !isEnvelope {
			exists, err := remoteBlobExists(ctx, repo, d)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("%w: missing destination payload %s", ErrNotFound, d.Digest)
			}
			return nil
		}
		raw, err := content.FetchAll(ctx, repo, d)
		if err != nil {
			return err
		}
		env, err := bundle.ParseEnvelope(d, raw)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrIntegrity, err)
		}
		for i, child := range env.Children {
			if err := walk(child, bundle.IsEnvelopeChild(d, i, child)); err != nil {
				return err
			}
			// Check the incoming layer role independently of envelope traversal:
			// the same digest may already have been visited as an index member.
			if (d.MediaType == v1.MediaTypeImageManifest || d.MediaType == bundle.DockerManifest) && i > 0 && bundle.IsEnvelope(child.MediaType) {
				exists, err := remoteBlobExists(ctx, repo, child)
				if err != nil {
					return err
				}
				if !exists {
					return fmt.Errorf("%w: missing destination envelope layer blob %s", ErrNotFound, child.Digest)
				}
			}
		}
		return nil
	}
	return walk(root, true)
}

// Blob existence is insufficient: ORAS Exists only resolves the digest and does
// not compare the expected size. HEAD validates reused bytes without fetching
// them. Payload media types remain opaque and are not part of blob identity.
func remoteBlobExists(ctx context.Context, repo any, expected v1.Descriptor) (bool, error) {
	blobs, ok := registryBlobs(repo)
	if !ok {
		return false, fmt.Errorf("%w: registry has no blob store", ErrUnsupported)
	}
	actual, err := blobs.Resolve(ctx, expected.Digest.String())
	if errors.Is(err, errdef.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if actual.Digest != expected.Digest || actual.Size != expected.Size {
		return false, fmt.Errorf("%w: blob descriptor digest/size mismatch for %s: expected size %d, registry reports %s size %d", ErrIntegrity, expected.Digest, expected.Size, actual.Digest, actual.Size)
	}
	return true, nil
}

func visitKey(d v1.Descriptor, isEnvelope bool) string {
	key := d.Digest.String()
	if isEnvelope {
		key += ":envelope:" + d.MediaType + ":" + d.ArtifactType
	}
	return key
}

// Inline envelope bytes carry the same graph semantics as fetched envelopes.
// Validate them before use without requiring a redundant source registry object.
func fetchEnvelopeBytes(ctx context.Context, src content.Fetcher, d v1.Descriptor) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.Data != nil {
		return content.ReadAll(bytes.NewReader(d.Data), d)
	}
	raw, err := content.FetchAll(ctx, src, d)
	if blobs, ok := registryBlobs(src); ok && errors.Is(err, errdef.ErrNotFound) {
		return content.FetchAll(ctx, blobs, d)
	}
	return raw, err
}
