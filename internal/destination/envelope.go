package destination

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/jsonutil"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func validateSubject(subject v1.Descriptor) error {
	if err := subject.Digest.Validate(); err != nil {
		return err
	}
	if !subject.Digest.Algorithm().Available() || subject.Size < 0 || !bundle.IsEnvelope(subject.MediaType) {
		return fmt.Errorf("invalid subject descriptor")
	}
	if subject.Data != nil {
		if _, err := bundle.ParseEnvelope(subject, subject.Data); err != nil {
			return err
		}
	}
	return nil
}
func matchRecordSubject(record Record, subject v1.Descriptor) error {
	ref, err := pinned(record.Destination)
	if err != nil {
		return err
	}
	if ref.Reference != subject.Digest.String() {
		return fmt.Errorf("record destination digest differs from subject")
	}
	return nil
}

// Prepare creates an immutable in-memory OCI attachment without reading a subject.
func Prepare(ctx context.Context, subject v1.Descriptor, record Record) (v1.Descriptor, content.ReadOnlyStorage, error) {
	fail := func(err error) (v1.Descriptor, content.ReadOnlyStorage, error) { return v1.Descriptor{}, nil, err }
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := validateSubject(subject); err != nil {
		return fail(err)
	}
	payload, err := Encode(record)
	if err != nil {
		return fail(err)
	}
	if err := matchRecordSubject(record, subject); err != nil {
		return fail(err)
	}
	store := memory.New()
	put := func(mt string, raw []byte) (v1.Descriptor, error) {
		desc := content.NewDescriptorFromBytes(mt, raw)
		return desc, store.Push(ctx, desc, bytes.NewReader(raw))
	}
	config, err := put(v1.MediaTypeEmptyJSON, []byte("{}"))
	if err != nil {
		return fail(err)
	}
	layer, err := put(ArtifactType, payload)
	if err != nil {
		return fail(err)
	}
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: ArtifactType, Config: config, Layers: []v1.Descriptor{layer}, Subject: &subject})
	if err != nil {
		return fail(err)
	}
	if int64(len(raw)) > MaxBytes {
		return fail(fmt.Errorf("destination manifest exceeds %d bytes", MaxBytes))
	}
	desc, err := put(v1.MediaTypeImageManifest, raw)
	if err != nil {
		return fail(err)
	}
	desc.ArtifactType = ArtifactType
	return desc, store, nil
}
func fetch(ctx context.Context, store content.ReadOnlyStorage, desc v1.Descriptor) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := desc.Digest.Validate(); err != nil {
		return nil, err
	}
	if !desc.Digest.Algorithm().Available() || desc.Size < 0 || desc.Size > MaxBytes {
		return nil, fmt.Errorf("invalid or oversized descriptor")
	}
	var raw []byte
	if desc.Data != nil {
		raw = append([]byte(nil), desc.Data...)
	} else {
		reader, err := store.Fetch(ctx, desc)
		if err != nil {
			return nil, err
		}
		raw, err = io.ReadAll(io.LimitReader(reader, desc.Size+1))
		closeErr := reader.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(raw)) != desc.Size || desc.Digest.Algorithm().FromBytes(raw) != desc.Digest {
		return nil, fmt.Errorf("descriptor size or digest mismatch")
	}
	return raw, nil
}

// Load verifies the exact OCI subject, empty config and single record layer.
// It never reads referenced source, destination, bundle or dependency content.
func Load(ctx context.Context, store content.ReadOnlyStorage, desc, subject v1.Descriptor) (Record, error) {
	fail := func(err error) (Record, error) {
		return Record{}, fmt.Errorf("destination record %s: %w", desc.Digest, err)
	}
	if err := validateSubject(subject); err != nil {
		return fail(err)
	}
	if desc.MediaType != v1.MediaTypeImageManifest {
		return fail(fmt.Errorf("must be an OCI manifest"))
	}
	if desc.ArtifactType != "" && desc.ArtifactType != ArtifactType {
		return fail(fmt.Errorf("descriptor artifactType differs"))
	}
	raw, err := fetch(ctx, store, desc)
	if err != nil {
		return fail(err)
	}
	if err := jsonutil.Validate(raw, true); err != nil {
		return fail(err)
	}
	if _, err := object(raw, []string{"schemaVersion", "config", "layers", "subject", "artifactType"}, "mediaType", "annotations"); err != nil {
		return fail(err)
	}
	env, err := bundle.ParseEnvelope(desc, raw)
	if err != nil {
		return fail(err)
	}
	if env.ArtifactType != ArtifactType {
		return fail(fmt.Errorf("unexpected artifactType"))
	}
	if env.Subject == nil || env.Subject.Digest != subject.Digest || env.Subject.Size != subject.Size || env.Subject.MediaType != subject.MediaType {
		return fail(fmt.Errorf("subject does not match"))
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fail(err)
	}
	if manifest.Config.MediaType != v1.MediaTypeEmptyJSON || manifest.Config.Size != 2 {
		return fail(fmt.Errorf("config must be the OCI empty JSON object"))
	}
	config, err := fetch(ctx, store, manifest.Config)
	if err != nil {
		return fail(err)
	}
	if !bytes.Equal(config, []byte("{}")) {
		return fail(fmt.Errorf("config must be the OCI empty JSON object"))
	}
	if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != ArtifactType {
		return fail(fmt.Errorf("expected exactly one destination record layer"))
	}
	payload, err := fetch(ctx, store, manifest.Layers[0])
	if err != nil {
		return fail(err)
	}
	record, err := Decode(payload)
	if err != nil {
		return fail(err)
	}
	if err := matchRecordSubject(record, subject); err != nil {
		return fail(err)
	}
	return record, nil
}
