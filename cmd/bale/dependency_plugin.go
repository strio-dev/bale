package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/plugin"
	"go.yaml.in/yaml/v3"
)

const pluginRuntimeOptions = `      --plugin-cache DIR  Private compiled cache (default: user cache/bale/plugins; empty disables)
      --plugin-memory N   Guest linear memory bytes, multiple of 65536 (default: 268435456)
      --plugin-timeout D  Execution timeout, excluding compilation (default: 30s)`

const dependencyPluginOptions = `      --plugin REF        OCI WASIp1 dependency producer (requires --input)
      --plugin-file FILE  Local Wasm producer for development (requires --input)
      --input FILE        Plugin-defined JSON request
` + pluginRuntimeOptions

func pluginOptions(opts commandOptions) (plugin.Options, error) {
	cache := opts.pluginCache
	if !opts.pluginCacheSet {
		base, err := os.UserCacheDir()
		if err != nil {
			return plugin.Options{}, err
		}
		cache = filepath.Join(base, "bale", "plugins")
	}
	return plugin.Options{ExecutionTimeout: opts.pluginTimeout, MemoryLimitBytes: opts.pluginMemory, CacheDirectory: cache}, nil
}

func configuredPluginRunner(opts commandOptions) bale.PluginRunner {
	return func(ctx context.Context, request bale.PluginRequest) (bale.DependencyDeclaration, error) {
		options, err := pluginOptions(opts)
		if err != nil {
			return bale.DependencyDeclaration{}, err
		}
		return plugin.Run(ctx, request, options)
	}
}

func readDependencySource(ctx context.Context, opts commandOptions) (bale.DependencyDeclaration, error) {
	if opts.file != "" {
		if opts.pluginOptionsSet {
			return bale.DependencyDeclaration{}, fmt.Errorf("--file cannot be combined with plugin options")
		}
		raw, err := os.ReadFile(opts.file)
		if err != nil {
			return bale.DependencyDeclaration{}, err
		}
		return bale.ParseDependencyDeclaration(raw)
	}
	if (opts.pluginRef == "") == (opts.pluginModule == "") || opts.pluginInput == "" {
		return bale.DependencyDeclaration{}, fmt.Errorf("provide --file or exactly one of --plugin and --plugin-file with --input")
	}
	if opts.pluginRef != "" {
		if err := explicitReference(opts.pluginRef, false); err != nil {
			return bale.DependencyDeclaration{}, fmt.Errorf("plugin reference: %w", err)
		}
	}
	if err := validateHTTP(opts.plainHTTP); err != nil {
		return bale.DependencyDeclaration{}, err
	}
	// Bound file reads before allocating the request. The SDK applies the same
	// bound to callers' byte slices before invoking the runtime.
	f, err := os.Open(opts.pluginInput)
	if err != nil {
		return bale.DependencyDeclaration{}, err
	}
	defer func() { _ = f.Close() }()
	const maxInput = 8 << 20
	info, err := f.Stat()
	if err != nil {
		return bale.DependencyDeclaration{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxInput {
		return bale.DependencyDeclaration{}, fmt.Errorf("plugin input must be a regular file at most %d bytes", maxInput)
	}
	input, err := io.ReadAll(io.LimitReader(f, maxInput+1))
	if err != nil {
		return bale.DependencyDeclaration{}, err
	}
	if len(input) > maxInput {
		return bale.DependencyDeclaration{}, fmt.Errorf("plugin input exceeds %d bytes", maxInput)
	}
	options, err := pluginOptions(opts)
	if err != nil {
		return bale.DependencyDeclaration{}, err
	}
	if opts.pluginRef == "" {
		return plugin.Run(ctx, plugin.Request{ModulePath: opts.pluginModule, Input: input}, options)
	}
	client, err := newCLIClient(opts, nil)
	if err != nil {
		return bale.DependencyDeclaration{}, err
	}
	result, err := plugin.RunOCI(ctx, client, plugin.OCIRequest{Ref: opts.pluginRef, Input: input}, options)
	return result.Declaration, err
}

func runDependencyGeneration(ctx context.Context, opts commandOptions, out, errOut io.Writer) int {
	if len(opts.args) != 0 {
		return fail(errOut, fmt.Errorf("generate takes no positional arguments; see 'bale dependencies generate --help'"))
	}
	declaration, err := readDependencySource(ctx, opts)
	if err != nil {
		return fail(errOut, err)
	}
	raw, err := dependencyYAML(declaration)
	if err != nil {
		return fail(errOut, err)
	}
	if _, err := out.Write(raw); err != nil {
		return fail(errOut, err)
	}
	return 0
}

func dependencyYAML(declaration bale.DependencyDeclaration) ([]byte, error) {
	// Use the existing registry codec to retain all selector fields, then omit
	// the registry-only version for the strict authoring YAML format.
	raw, err := bale.EncodeDependencyDeclaration(declaration)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	delete(fields, "schemaVersion")
	data, err := yaml.Marshal(fields)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > bale.DependencyMaxBytes {
		return nil, fmt.Errorf("generated dependency YAML exceeds %d bytes", bale.DependencyMaxBytes)
	}
	return data, nil
}
