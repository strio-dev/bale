// Package pluginoci reads and writes archive-based OCI discovery plugins.
// Distribution packaging is independent of the portable WASIp1 execution ABI.
package pluginoci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

const (
	ArtifactType     = "application/vnd.bale.plugin.v1+json"
	MaxModuleBytes   = 128 << 20
	MaxMetadataBytes = 4 << 20
	MaxArchiveBytes  = 256 << 20
)

var coreHeader = []byte{0, 'a', 's', 'm', 1, 0, 0, 0}

// Module owns a verified, private temporary copy of a portable module.
type Module struct {
	Path string
	dir  string
}

// Close removes the temporary module. It is safe to call more than once.
func (m *Module) Close() error {
	if m == nil || m.dir == "" {
		return nil
	}
	return os.RemoveAll(m.dir)
}

// Open selects one wasm/wasip1 package, verifies its metadata and archive, and
// materializes only plugin.wasm. It never executes package metadata as commands.
// Callers retain responsibility for preserving the original root graph.
func Open(ctx context.Context, store content.ReadOnlyStorage, descriptor v1.Descriptor) (_ *Module, err error) {
	if store == nil {
		return nil, errors.New("plugin storage is required")
	}
	if descriptor.MediaType == v1.MediaTypeImageIndex {
		data, err := fetchMetadata(ctx, store, descriptor)
		if err != nil {
			return nil, err
		}
		if err := validateEnvelopeMediaType(data, v1.MediaTypeImageIndex); err != nil {
			return nil, err
		}
		var index v1.Index
		if err := json.Unmarshal(data, &index); err != nil {
			return nil, fmt.Errorf("plugin index: %w", err)
		}
		if index.SchemaVersion != 2 {
			return nil, errors.New("unsupported plugin index envelope")
		}
		var selected []v1.Descriptor
		for _, child := range index.Manifests {
			if child.Platform != nil && child.Platform.Architecture == "wasm" && child.Platform.OS == "wasip1" {
				selected = append(selected, child)
			}
		}
		if len(selected) != 1 {
			return nil, fmt.Errorf("plugin index requires exactly one wasm/wasip1 manifest, found %d", len(selected))
		}
		descriptor = selected[0]
	}
	if descriptor.MediaType != v1.MediaTypeImageManifest {
		return nil, fmt.Errorf("unsupported plugin manifest media type %q", descriptor.MediaType)
	}
	data, err := fetchMetadata(ctx, store, descriptor)
	if err != nil {
		return nil, err
	}
	if err := validateEnvelopeMediaType(data, v1.MediaTypeImageManifest); err != nil {
		return nil, err
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("plugin manifest: %w", err)
	}
	if manifest.SchemaVersion != 2 || manifest.Subject != nil || manifest.ArtifactType != ArtifactType {
		return nil, errors.New("unsupported discovery plugin manifest")
	}
	if manifest.Config.MediaType != v1.MediaTypeEmptyJSON {
		return nil, errors.New("plugin requires an empty OCI config")
	}
	config, err := fetchMetadata(ctx, store, manifest.Config)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(config, []byte("{}")) {
		return nil, errors.New("plugin config must be the empty JSON object")
	}
	if len(manifest.Layers) < 1 || len(manifest.Layers) > 2 || manifest.Layers[0].MediaType != v1.MediaTypeImageLayerGzip {
		return nil, errors.New("plugin requires one gzip package and optional provenance")
	}
	archive := manifest.Layers[0]
	if err := validateDescriptor(archive, MaxArchiveBytes); err != nil {
		return nil, fmt.Errorf("plugin archive: %w", err)
	}
	title := archive.Annotations[v1.AnnotationTitle]
	if len(manifest.Layers) == 2 {
		provenance := manifest.Layers[1]
		if title == "" || provenance.Annotations[v1.AnnotationTitle] != title+".prov" {
			return nil, errors.New("plugin provenance title must match its package")
		}
		// Verify sidecar bytes, without interpreting them as a trusted signature.
		if _, err := fetchMetadata(ctx, store, provenance); err != nil {
			return nil, fmt.Errorf("plugin provenance: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "bale-plugin-*")
	if err != nil {
		return nil, err
	}
	module := &Module{Path: filepath.Join(dir, "module.wasm"), dir: dir}
	defer func() {
		if err != nil {
			_ = module.Close()
		}
	}()
	file, err := os.OpenFile(module.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	reader, err := fetchReader(ctx, store, archive)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	verified := content.NewVerifyReader(contextReader{ctx, reader}, archive)
	metadata, scanErr := readArchive(ctx, verified, file)
	if scanErr == nil && metadata.title() != title {
		scanErr = errors.New("plugin archive title does not match package name and version")
	}
	err = errors.Join(scanErr, verified.Verify(), reader.Close(), file.Close())
	if err != nil {
		return nil, fmt.Errorf("plugin archive: %w", err)
	}
	return module, nil
}

// Pack publishes an existing discovery .tgz unchanged using OCI archive layers.
// Package creation is owned by build tooling, not by this transport codec.
func Pack(ctx context.Context, store content.Storage, packagePath string) (v1.Descriptor, error) {
	if store == nil {
		return v1.Descriptor{}, errors.New("plugin storage is required")
	}
	file, err := os.Open(packagePath)
	if err != nil {
		return v1.Descriptor{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return v1.Descriptor{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxArchiveBytes || info.Size() < 1 {
		return v1.Descriptor{}, errors.New("plugin package must be a regular gzip archive of at most 256 MiB")
	}
	digester := digest.SHA256.Digester()
	metadata, err := readArchive(ctx, io.TeeReader(file, digester.Hash()), io.Discard)
	if err != nil {
		return v1.Descriptor{}, fmt.Errorf("plugin package: %w", err)
	}
	blob := v1.Descriptor{MediaType: v1.MediaTypeImageLayerGzip, Digest: digester.Digest(), Size: info.Size(), Annotations: map[string]string{v1.AnnotationTitle: metadata.title()}}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return v1.Descriptor{}, err
	}
	verified := content.NewVerifyReader(contextReader{ctx, file}, blob)
	pushErr := store.Push(ctx, blob, verified)
	if pushErr != nil && !errors.Is(pushErr, errdef.ErrAlreadyExists) {
		return v1.Descriptor{}, pushErr
	}
	// Stores may skip existing blobs without reading. Verify the remaining source
	// as well, detecting mutation between validation/hashing and publication.
	if _, err := io.Copy(io.Discard, verified); err != nil {
		return v1.Descriptor{}, err
	}
	if err := verified.Verify(); err != nil {
		return v1.Descriptor{}, fmt.Errorf("plugin package changed while packing: %w", err)
	}
	return oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, ArtifactType, oras.PackManifestOptions{
		Layers:              []v1.Descriptor{blob},
		ManifestAnnotations: map[string]string{v1.AnnotationCreated: "1970-01-01T00:00:01Z"},
	})
}

func fetchReader(ctx context.Context, store content.ReadOnlyStorage, desc v1.Descriptor) (io.ReadCloser, error) {
	if desc.Data != nil {
		return io.NopCloser(bytes.NewReader(desc.Data)), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return store.Fetch(ctx, desc)
}

func validateDescriptor(desc v1.Descriptor, limit int64) error {
	if desc.Size < 0 || desc.Size > limit {
		return fmt.Errorf("descriptor size %d exceeds supported range 0..%d", desc.Size, limit)
	}
	if err := desc.Digest.Validate(); err != nil {
		return fmt.Errorf("invalid digest: %w", err)
	}
	if !desc.Digest.Algorithm().Available() {
		return errors.New("unsupported digest algorithm")
	}
	return nil
}

func fetchMetadata(ctx context.Context, store content.ReadOnlyStorage, desc v1.Descriptor) ([]byte, error) {
	if err := validateDescriptor(desc, MaxMetadataBytes); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if desc.Data != nil {
		return content.ReadAll(contextReader{ctx, bytes.NewReader(desc.Data)}, desc)
	}
	return content.FetchAll(ctx, store, desc)
}

// OCI permits omission of the envelope mediaType; a present value must agree.
func validateEnvelopeMediaType(raw []byte, expected string) error {
	var envelope struct {
		MediaType json.RawMessage `json:"mediaType"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if len(envelope.MediaType) == 0 {
		return nil
	}
	var value *string
	if err := json.Unmarshal(envelope.MediaType, &value); err != nil || value == nil {
		return errors.New("plugin envelope mediaType must be a string")
	}
	if *value != expected {
		return errors.New("plugin envelope mediaType conflicts with descriptor")
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
