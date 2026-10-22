package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/strio-dev/bale/internal/jsonutil"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
)

func validateDescriptor(d ocispec.Descriptor) error {
	if err := d.Digest.Validate(); err != nil {
		return fmt.Errorf("descriptor digest: %w", err)
	}
	if !d.Digest.Algorithm().Available() {
		return fmt.Errorf("unsupported digest algorithm %q", d.Digest.Algorithm())
	}
	if d.Size < 0 || d.MediaType == "" {
		return fmt.Errorf("descriptor needs a media type and nonnegative size")
	}
	if d.Data != nil {
		return verifyBytes(d, d.Data)
	}
	return nil
}

func verifyBytes(d ocispec.Descriptor, raw []byte) error {
	if int64(len(raw)) != d.Size {
		return fmt.Errorf("%s: size mismatch", d.Digest)
	}
	if err := d.Digest.Validate(); err != nil {
		return err
	}
	if !d.Digest.Algorithm().Available() {
		return fmt.Errorf("unsupported digest algorithm %q", d.Digest.Algorithm())
	}
	if d.Digest.Algorithm().FromBytes(raw) != d.Digest {
		return fmt.Errorf("%s: digest mismatch", d.Digest)
	}
	return nil
}

// ParseEnvelope verifies the descriptor and recognizes only supported OCI and
// Docker schema-2 graph envelopes. Child interpretation follows IsEnvelopeChild.
func ParseEnvelope(desc ocispec.Descriptor, raw []byte) (Envelope, error) {
	if err := validateDescriptor(desc); err != nil {
		return Envelope{}, err
	}
	if err := verifyBytes(desc, raw); err != nil {
		return Envelope{}, err
	}
	if !IsEnvelope(desc.MediaType) {
		return Envelope{}, fmt.Errorf("unsupported envelope media type %q", desc.MediaType)
	}
	if err := uniqueJSON(raw, false); err != nil {
		return Envelope{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Envelope{}, err
	}
	var schema int
	if err := json.Unmarshal(fields["schemaVersion"], &schema); err != nil || schema != 2 {
		return Envelope{}, fmt.Errorf("envelope schemaVersion must be 2")
	}
	if v, ok := fields["mediaType"]; ok {
		var mt string
		if err := json.Unmarshal(v, &mt); err != nil || mt != desc.MediaType {
			return Envelope{}, fmt.Errorf("envelope mediaType conflicts with descriptor")
		}
	}
	env := Envelope{Children: []ocispec.Descriptor{}}
	if value, ok := fields["artifactType"]; ok {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return Envelope{}, fmt.Errorf("envelope artifactType must be a string")
		}
		if err := json.Unmarshal(value, &env.ArtifactType); err != nil {
			return Envelope{}, fmt.Errorf("envelope artifactType must be a string: %w", err)
		}
	}
	if v, ok := fields["subject"]; ok {
		if err := descriptorFields(v); err != nil {
			return Envelope{}, fmt.Errorf("subject: %w", err)
		}
		var subject ocispec.Descriptor
		if err := json.Unmarshal(v, &subject); err != nil {
			return Envelope{}, fmt.Errorf("subject: %w", err)
		}
		if err := validateDescriptor(subject); err != nil {
			return Envelope{}, fmt.Errorf("subject: %w", err)
		}
		env.Subject = &subject
	}
	if isIndex(desc.MediaType) {
		if _, ok := fields["config"]; ok {
			return Envelope{}, fmt.Errorf("index cannot contain config")
		}
		if _, ok := fields["layers"]; ok {
			return Envelope{}, fmt.Errorf("index cannot contain layers")
		}
		v, ok := fields["manifests"]
		if !ok || !bytes.HasPrefix(bytes.TrimSpace(v), []byte("[")) {
			return Envelope{}, fmt.Errorf("index manifests must be an array")
		}
		if err := json.Unmarshal(v, &env.Children); err != nil {
			return Envelope{}, fmt.Errorf("manifests: %w", err)
		}
		if err := descriptorArrayFields(v); err != nil {
			return Envelope{}, err
		}
	} else {
		if _, ok := fields["manifests"]; ok {
			return Envelope{}, fmt.Errorf("manifest cannot contain manifests")
		}
		var cfg ocispec.Descriptor
		v, ok := fields["config"]
		if !ok {
			return Envelope{}, fmt.Errorf("manifest config is required")
		}
		if err := descriptorFields(v); err != nil {
			return Envelope{}, fmt.Errorf("config: %w", err)
		}
		if err := json.Unmarshal(v, &cfg); err != nil {
			return Envelope{}, fmt.Errorf("config: %w", err)
		}
		var layers []ocispec.Descriptor
		v, ok = fields["layers"]
		if !ok || !bytes.HasPrefix(bytes.TrimSpace(v), []byte("[")) {
			return Envelope{}, fmt.Errorf("manifest layers must be an array")
		}
		if err := json.Unmarshal(v, &layers); err != nil {
			return Envelope{}, fmt.Errorf("layers: %w", err)
		}
		if err := descriptorArrayFields(v); err != nil {
			return Envelope{}, err
		}
		env.Children = append([]ocispec.Descriptor{cfg}, layers...)
		if desc.MediaType == ocispec.MediaTypeImageManifest && env.ArtifactType == "" {
			env.ArtifactType = cfg.MediaType
		}
	}
	if desc.ArtifactType != "" && desc.ArtifactType != env.ArtifactType {
		return Envelope{}, fmt.Errorf("envelope artifactType conflicts with descriptor")
	}
	for _, child := range env.Children {
		if err := validateDescriptor(child); err != nil {
			return Envelope{}, err
		}
	}
	return env, nil
}

func descriptorArrayFields(raw []byte) error {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	for _, value := range values {
		if err := descriptorFields(value); err != nil {
			return err
		}
	}
	return nil
}

func descriptorFields(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, key := range []string{"mediaType", "digest", "size"} {
		value, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("descriptor %s is required", key)
		}
	}
	return nil
}

func fetchBytes(ctx context.Context, store content.ReadOnlyStorage, d ocispec.Descriptor) ([]byte, error) {
	if err := validateDescriptor(d); err != nil {
		return nil, err
	}
	if d.Data != nil {
		return append([]byte(nil), d.Data...), nil
	}
	return content.FetchAll(ctx, store, d)
}

// VerifyClosure reads and verifies every strong child, even when the root is
// already present. Merely checking existence would hide interrupted transfers.
func VerifyClosure(ctx context.Context, store content.ReadOnlyStorage, root ocispec.Descriptor) error {
	_, err := walkClosure(ctx, store, root)
	return err
}

func walkClosure(ctx context.Context, store content.ReadOnlyStorage, root ocispec.Descriptor) (map[digest.Digest]ocispec.Descriptor, error) {
	return walkGraph(ctx, store, root, nil)
}

func walkGraph(ctx context.Context, store content.ReadOnlyStorage, root ocispec.Descriptor, inventory map[digest.Digest]ocispec.Descriptor) (map[digest.Digest]ocispec.Descriptor, error) {
	return walkGraphWithPayloads(ctx, store, root, inventory, true)
}

func walkGraphWithPayloads(ctx context.Context, store content.ReadOnlyStorage, root ocispec.Descriptor, inventory map[digest.Digest]ocispec.Descriptor, verifyPayloads bool) (map[digest.Digest]ocispec.Descriptor, error) {
	seen := map[digest.Digest]ocispec.Descriptor{}
	expanded := map[string]bool{}
	var walk func(ocispec.Descriptor, bool) error
	walk = func(d ocispec.Descriptor, envelope bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateDescriptor(d); err != nil {
			return err
		}
		interpretation := d.Digest.String() + "\x00" + d.MediaType + "\x00" + d.ArtifactType
		if old, ok := seen[d.Digest]; ok {
			if !sameDescriptor(old, d) {
				return fmt.Errorf("conflicting descriptors for %s", d.Digest)
			}
			if envelope && !IsEnvelope(d.MediaType) {
				return fmt.Errorf("unsupported envelope media type %q", d.MediaType)
			}
			// A digest may first occur as an opaque config/layer and later as
			// an envelope edge. Its bytes were verified, but its graph was not.
			if !envelope || expanded[interpretation] {
				return nil
			}
		}
		seen[d.Digest] = d
		if inventory != nil {
			actual, ok := inventory[d.Digest]
			if !ok || !sameDescriptor(actual, d) {
				return fmt.Errorf("verified source inventory is missing %s", d.Digest)
			}
		}
		if !envelope {
			if inventory != nil || !verifyPayloads {
				return nil
			}
			if d.Data != nil {
				return nil
			}
			rc, err := store.Fetch(ctx, d)
			if err != nil {
				return fmt.Errorf("fetch %s: %w", d.Digest, err)
			}
			verifier := d.Digest.Verifier()
			n, copyErr := io.Copy(verifier, io.LimitReader(rc, d.Size+1))
			closeErr := rc.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if n != d.Size || !verifier.Verified() {
				return fmt.Errorf("%s: payload size or digest mismatch", d.Digest)
			}
			return nil
		}
		raw, err := fetchBytes(ctx, store, d)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", d.Digest, err)
		}
		env, err := ParseEnvelope(d, raw)
		if err != nil {
			return err
		}
		expanded[interpretation] = true
		for i, child := range env.Children {
			if err := walk(child, IsEnvelopeChild(d, i, child)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, true); err != nil {
		return nil, err
	}
	return seen, nil
}

func sameDescriptor(a, b ocispec.Descriptor) bool {
	// Media types describe interpretation at an edge, not content identity.
	// Envelope interpretations are validated separately on every distinct edge.
	return a.Digest == b.Digest && a.Size == b.Size
}

func uniqueJSON(raw []byte, rejectNull bool) error {
	return jsonutil.Validate(raw, rejectNull)
}
