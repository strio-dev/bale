package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/pluginoci"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

func recipeHasPlugins(recipe config.Recipe) bool {
	for _, input := range recipe.Include {
		if input.Plugin != nil {
			return true
		}
	}
	return false
}

func (g *graph) generated(ctx context.Context, d v1.Descriptor) (bool, error) {
	if g.local == nil {
		return false, nil
	}
	return g.local.Exists(ctx, d)
}

// overlayStorage serves only locally generated content ahead of the original
// source. Registry inventories remain supplied by the original provider.
type overlayStorage struct {
	local  *metadataStore
	source content.ReadOnlyStorage
}

func (s overlayStorage) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	if ok, err := s.local.Exists(ctx, d); err != nil {
		return nil, err
	} else if ok {
		return s.local.Fetch(ctx, d)
	}
	if blobs, ok := registryBlobs(s.source); ok && !bundle.IsEnvelope(d.MediaType) {
		return blobs.Fetch(ctx, d)
	}
	return s.source.Fetch(ctx, d)
}
func (s overlayStorage) Exists(ctx context.Context, d v1.Descriptor) (bool, error) {
	if ok, err := s.local.Exists(ctx, d); err != nil || ok {
		return ok, err
	}
	if _, ok := registryBlobs(s.source); ok && !bundle.IsEnvelope(d.MediaType) {
		return remoteBlobExists(ctx, s.source, d)
	}
	return s.source.Exists(ctx, d)
}

// Keep normal registry payload capture lazy even when generated metadata uses
// an overlay. Only locally generated blobs and caller stores need local reads.
func captureBlobExists(ctx context.Context, store content.ReadOnlyStorage, d v1.Descriptor) (bool, int64, error) {
	if overlay, ok := store.(overlayStorage); ok {
		exists, err := overlay.local.Exists(ctx, d)
		if err != nil || exists {
			return exists, 0, err
		}
		return captureBlobExists(ctx, overlay.source, d)
	}
	if _, ok := registryBlobs(store); ok {
		exists, err := remoteBlobExists(ctx, store, d)
		return exists, 0, err
	}
	raw, err := content.FetchAll(ctx, store, d)
	if err != nil {
		return false, 0, err
	}
	return true, int64(len(raw)), nil
}

func (g *graph) pluginInput(ctx context.Context, root bundle.Root, plugin *config.Plugin) (json.RawMessage, error) {
	input := map[string]json.RawMessage{}
	if len(plugin.Input) > 0 {
		if err := json.Unmarshal(plugin.Input, &input); err != nil {
			return nil, err
		}
	}
	if input == nil {
		return nil, fmt.Errorf("%w: plugin input must be an object", ErrInvalid)
	}
	initial, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	requestSize := len(initial)
	if requestSize > config.MaxPluginInputBytes {
		return nil, fmt.Errorf("%w: plugin request exceeds %d bytes", ErrUnsupported, config.MaxPluginInputBytes)
	}
	if len(plugin.Blobs) > 0 {
		if root.Descriptor.MediaType != v1.MediaTypeImageManifest {
			return nil, fmt.Errorf("%w: plugin blob bindings require an OCI image manifest", ErrUnsupported)
		}
		store, err := g.store(ctx, root.Repository)
		if err != nil {
			return nil, err
		}
		raw, err := fetchEnvelopeBytes(ctx, store, root.Descriptor)
		if err != nil {
			return nil, err
		}
		var manifest v1.Manifest
		if err = json.Unmarshal(raw, &manifest); err != nil {
			return nil, err
		}
		for name, binding := range plugin.Blobs {
			if _, exists := input[name]; exists {
				return nil, fmt.Errorf("%w: plugin input binding %q collides with literal input", ErrInvalid, name)
			}
			var matches []v1.Descriptor
			for _, layer := range manifest.Layers {
				if layer.MediaType == binding.MediaType {
					matches = append(matches, layer)
				}
			}
			if len(matches) != 1 {
				return nil, fmt.Errorf("%w: plugin binding %q requires exactly one %s layer; found %d", ErrInvalid, name, binding.MediaType, len(matches))
			}
			layer := matches[0]
			if layer.Size < 0 || layer.Size > config.MaxPluginInputBytes || base64.StdEncoding.EncodedLen(int(layer.Size)) > config.MaxPluginInputBytes {
				return nil, fmt.Errorf("%w: plugin binding exceeds request limit", ErrUnsupported)
			}
			keyJSON, err := json.Marshal(name)
			if err != nil {
				return nil, err
			}
			addition := len(keyJSON) + 1 + base64.StdEncoding.EncodedLen(int(layer.Size)) + 2
			if len(input) > 0 {
				addition++
			}
			if addition > config.MaxPluginInputBytes-requestSize {
				return nil, fmt.Errorf("%w: combined plugin request exceeds %d bytes", ErrUnsupported, config.MaxPluginInputBytes)
			}
			requestSize += addition
			var data []byte
			if layer.Data != nil {
				data, err = content.ReadAll(bytes.NewReader(layer.Data), layer)
			} else {
				data, err = content.FetchAll(ctx, store, layer)
			}
			if err != nil {
				return nil, err
			}
			input[name], err = json.Marshal(base64.StdEncoding.EncodeToString(data))
			if err != nil {
				return nil, err
			}
		}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if len(raw) > config.MaxPluginInputBytes {
		return nil, fmt.Errorf("%w: plugin request exceeds %d bytes", ErrUnsupported, config.MaxPluginInputBytes)
	}
	return raw, nil
}

func (g *graph) capturePlugins(ctx context.Context, resolved map[string]v1.Descriptor) error {
	seen := map[string]bool{}
	for _, root := range g.meta.Roots {
		plugin := g.meta.Recipe.Include[root.Input].Plugin
		if plugin == nil {
			continue
		}
		key := fmt.Sprintf("%d/%s@%s", root.Input, root.Repository, root.Descriptor.Digest)
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := g.capturePlugin(ctx, root, plugin, resolved); err != nil {
			return err
		}
	}
	return nil
}

func cacheGenerated(ctx context.Context, target *metadataStore, source content.ReadOnlyStorage, d v1.Descriptor) error {
	raw, err := content.FetchAll(ctx, source, d)
	if err != nil {
		return err
	}
	if err = target.Push(ctx, d, bytes.NewReader(raw)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return err
	}
	if bundle.IsEnvelope(d.MediaType) {
		env, err := bundle.ParseEnvelope(d, raw)
		if err != nil {
			return err
		}
		for _, child := range env.Children {
			if err := cacheGenerated(ctx, target, source, child); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *graph) capturePlugin(ctx context.Context, root bundle.Root, plugin *config.Plugin, resolved map[string]v1.Descriptor) (err error) {
	defer func() {
		if err != nil {
			err = selectionError(root.Input, fullReference(root.Repository, root.Reference), root.Descriptor, plugin, err)
		}
	}()
	ref, err := parseRef(plugin.Ref)
	if err != nil {
		return err
	}
	repository := repositoryName(ref)
	descriptor := resolved[repository+"@"+ref.Reference]
	store, err := g.store(ctx, repository)
	if err != nil {
		return err
	}
	g.service.recipeProgress("fetching-plugin", root.Input, fullReference(root.Repository, root.Reference), root.Repository, root.Descriptor, plugin)
	module, err := pluginoci.Open(ctx, store, descriptor)
	if err != nil {
		return fmt.Errorf("fetch plugin module: %w", err)
	}
	input, err := g.pluginInput(ctx, root, plugin)
	if err != nil {
		return fmt.Errorf("bind plugin input: %w", errors.Join(err, module.Close()))
	}
	g.service.recipeProgress("running-plugin", root.Input, fullReference(root.Repository, root.Reference), root.Repository, root.Descriptor, plugin)
	declaration, runErr := g.service.pluginRunner(ctx, PluginRequest{ModulePath: module.Path, Input: input})
	if err := errors.Join(runErr, module.Close()); err != nil {
		return fmt.Errorf("run plugin: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	prepared, err := PrepareDependencyDeclaration(ctx, Root{Repository: root.Repository, Descriptor: root.Descriptor}, declaration)
	if err != nil {
		return fmt.Errorf("prepare plugin declaration: %w", err)
	}
	if g.local == nil {
		g.local = newMetadataStore()
	}
	if err := cacheGenerated(ctx, g.local, prepared.Store, prepared.Descriptor); err != nil {
		return fmt.Errorf("capture plugin declaration: %w", err)
	}
	// Freeze the actual source inventory before adding a local candidate. Multiple
	// profiles for the same subject remain available for explicit digest selection.
	refs, err := g.dependencies.inventory(ctx, root.Repository, root.Descriptor)
	if err != nil {
		return fmt.Errorf("discover subject referrers: %w", err)
	}
	inventoryKey := root.Repository + "@" + root.Descriptor.Digest.String()
	found := false
	for _, d := range refs {
		if d.Digest == prepared.Descriptor.Digest {
			found = true
		}
	}
	if !found {
		g.dependencies.inventories[inventoryKey] = append(refs, prepared.Descriptor)
	}
	delete(g.dependencies.candidates, inventoryKey)
	if err := g.visit(ctx, repository, descriptor, false, true); err != nil {
		return fmt.Errorf("capture plugin graph: %w", err)
	}
	if err := g.referrer(ctx, root.Repository, root.Descriptor, prepared.Descriptor, false); err != nil {
		return fmt.Errorf("associate plugin declaration: %w", err)
	}
	g.meta.Plugins = append(g.meta.Plugins, bundle.PluginResolution{Input: root.Input, Repository: root.Repository, Subject: root.Descriptor, Ref: plugin.Ref, Descriptor: descriptor, Declaration: prepared.Descriptor})
	return nil
}
