// Package pluginoci reads and writes portable WASI command modules using the
// CNCF Wasm OCI image profile. It does not interpret compiled runtime caches.
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
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

const (
	MediaTypeConfig  = "application/vnd.wasm.config.v0+json"
	MediaTypeModule  = "application/wasm"
	MaxModuleBytes   = 128 << 20
	MaxMetadataBytes = 4 << 20
)

var coreHeader = []byte{0, 'a', 's', 'm', 1, 0, 0, 0}

type moduleConfig struct {
	Architecture string          `json:"architecture"`
	OS           string          `json:"os"`
	LayerDigests []digest.Digest `json:"layerDigests"`
}

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

// Open selects exactly one wasm/wasip1 manifest and verifies its profile and
// module content. Callers retain responsibility for preserving the root graph.
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
	if manifest.SchemaVersion != 2 {
		return nil, errors.New("unsupported plugin manifest envelope")
	}
	if manifest.Config.MediaType != MediaTypeConfig {
		return nil, fmt.Errorf("unsupported plugin config media type %q", manifest.Config.MediaType)
	}
	if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != MediaTypeModule {
		return nil, errors.New("plugin requires exactly one application/wasm layer")
	}
	blob := manifest.Layers[0]
	if err := validateDescriptor(blob, MaxModuleBytes); err != nil {
		return nil, fmt.Errorf("plugin module: %w", err)
	}
	data, err = fetchMetadata(ctx, store, manifest.Config)
	if err != nil {
		return nil, err
	}
	var config moduleConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("plugin config: %w", err)
	}
	if config.Architecture != "wasm" || config.OS != "wasip1" {
		return nil, errors.New("plugin config requires architecture wasm and os wasip1")
	}
	if len(config.LayerDigests) != 1 || config.LayerDigests[0] != blob.Digest {
		return nil, errors.New("plugin config layerDigests must match manifest layers in order")
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
	var reader io.ReadCloser
	if blob.Data != nil {
		reader = io.NopCloser(bytes.NewReader(blob.Data))
	} else {
		reader, err = store.Fetch(ctx, blob)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	verified := content.NewVerifyReader(contextReader{ctx, reader}, blob)
	_, copyErr := io.Copy(file, verified)
	verifyErr := verified.Verify()
	readCloseErr := reader.Close()
	closeErr := file.Close()
	if err := errors.Join(copyErr, verifyErr, readCloseErr, closeErr); err != nil {
		return nil, fmt.Errorf("plugin module: %w", err)
	}
	file, err = os.Open(module.Path)
	if err != nil {
		return nil, err
	}
	err = checkHeader(file)
	err = errors.Join(err, file.Close())
	if err != nil {
		return nil, err
	}
	return module, nil
}

// Pack writes a deterministic OCI image containing one portable WASIp1 module.
// The caller owns storage and publication of the resulting manifest.
func Pack(ctx context.Context, store content.Storage, modulePath string) (v1.Descriptor, error) {
	if store == nil {
		return v1.Descriptor{}, errors.New("plugin storage is required")
	}
	file, err := os.Open(modulePath)
	if err != nil {
		return v1.Descriptor{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return v1.Descriptor{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxModuleBytes || info.Size() < int64(len(coreHeader)) {
		return v1.Descriptor{}, errors.New("plugin must be a regular core Wasm module of at most 128 MiB")
	}
	if err := checkHeader(file); err != nil {
		return v1.Descriptor{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return v1.Descriptor{}, err
	}
	digester := digest.SHA256.Digester()
	n, err := io.Copy(digester.Hash(), io.LimitReader(contextReader{ctx, file}, info.Size()+1))
	if err != nil {
		return v1.Descriptor{}, err
	}
	if n != info.Size() {
		return v1.Descriptor{}, errors.New("plugin file changed while hashing")
	}
	blob := v1.Descriptor{MediaType: MediaTypeModule, Digest: digester.Digest(), Size: n}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return v1.Descriptor{}, err
	}
	verified := content.NewVerifyReader(contextReader{ctx, file}, blob)
	pushErr := store.Push(ctx, blob, verified)
	if pushErr != nil && !errors.Is(pushErr, errdef.ErrAlreadyExists) {
		return v1.Descriptor{}, pushErr
	}
	// A store may skip an existing blob without reading the source. Verify the
	// remaining bytes even in that case, detecting modifications after hashing.
	if _, err := io.Copy(io.Discard, verified); err != nil {
		return v1.Descriptor{}, err
	}
	if err := verified.Verify(); err != nil {
		return v1.Descriptor{}, fmt.Errorf("plugin file changed while packing: %w", err)
	}
	config, err := pushJSON(ctx, store, MediaTypeConfig, moduleConfig{Architecture: "wasm", OS: "wasip1", LayerDigests: []digest.Digest{blob.Digest}})
	if err != nil {
		return v1.Descriptor{}, err
	}
	return pushJSON(ctx, store, v1.MediaTypeImageManifest, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{blob}})
}

func pushJSON(ctx context.Context, store content.Storage, mediaType string, value any) (v1.Descriptor, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return v1.Descriptor{}, err
	}
	desc := content.NewDescriptorFromBytes(mediaType, data)
	if err := store.Push(ctx, desc, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return v1.Descriptor{}, err
	}
	return desc, nil
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

func checkHeader(reader io.Reader) error {
	var header [8]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return fmt.Errorf("plugin core Wasm header: %w", err)
	}
	if !bytes.Equal(header[:], coreHeader) {
		return errors.New("plugin requires a portable core Wasm module, not a component or native cache")
	}
	return nil
}

// OCI mediaType is optional in the JSON envelope because the descriptor already
// supplies it. A present value still has to be a string consistent with it.
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
