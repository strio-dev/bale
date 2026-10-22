package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

// PrepareDependencyDeclaration prepares a local, deterministic inclusion attachment.
func PrepareDependencyDeclaration(ctx context.Context, subject Root, declaration dependency.Declaration) (AttachRequest, error) {
	fail := func(kind, cause error) (AttachRequest, error) {
		return AttachRequest{}, &Error{Kind: kind, Op: "prepare dependency declaration", Repository: subject.Repository, Descriptor: subject.Descriptor, Cause: cause}
	}
	if err := ctx.Err(); err != nil {
		return fail(nil, err)
	}
	d := subject.Descriptor
	if err := d.Digest.Validate(); err != nil {
		return fail(ErrInvalid, fmt.Errorf("subject descriptor digest: %w", err))
	}
	if d.Size < 0 || !d.Digest.Algorithm().Available() {
		return fail(ErrInvalid, fmt.Errorf("subject descriptor needs a nonnegative size and supported digest algorithm"))
	}
	if !bundle.IsEnvelope(d.MediaType) {
		return fail(ErrUnsupported, fmt.Errorf("subject must be a supported OCI or Docker envelope"))
	}
	reference := subject.Repository + "@" + d.Digest.String()
	if err := dependency.ValidateReference(reference); err != nil {
		return fail(ErrInvalid, fmt.Errorf("subject repository: %w", err))
	}
	if d.Data != nil {
		if _, err := bundle.ParseEnvelope(d, d.Data); err != nil {
			return fail(ErrInvalid, fmt.Errorf("subject inline data: %w", err))
		}
	}
	payload, err := dependency.Encode(declaration)
	if err != nil {
		return fail(ErrInvalid, err)
	}
	store := memory.New()
	put := func(mediaType string, raw []byte) (v1.Descriptor, error) {
		desc := content.NewDescriptorFromBytes(mediaType, raw)
		err := store.Push(ctx, desc, bytes.NewReader(raw))
		return desc, err
	}
	config, err := put(v1.MediaTypeEmptyJSON, []byte("{}"))
	if err != nil {
		return fail(nil, err)
	}
	layer, err := put(dependency.ArtifactType, payload)
	if err != nil {
		return fail(nil, err)
	}
	manifest := v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest,
		ArtifactType: dependency.ArtifactType, Config: config, Layers: []v1.Descriptor{layer}, Subject: &d,
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return fail(ErrInvalid, err)
	}
	if int64(len(raw)) > dependency.MaxBytes {
		return fail(ErrInvalid, fmt.Errorf("dependency manifest exceeds %d bytes", dependency.MaxBytes))
	}
	descriptor, err := put(v1.MediaTypeImageManifest, raw)
	if err != nil {
		return fail(nil, err)
	}
	descriptor.ArtifactType = dependency.ArtifactType
	return AttachRequest{Subject: reference, Descriptor: descriptor, Store: store}, nil
}
