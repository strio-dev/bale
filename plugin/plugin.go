// Package plugin runs portable WASIp1 dependency producers. Plugins receive
// plugin-defined JSON on stdin and return Bale dependency declaration JSON on
// stdout. The runtime and its native cache are private implementation details.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/internal/pluginoci"
	"github.com/strio-dev/bale/internal/wasm"
	"oras.land/oras-go/v2/content"
)

// Request identifies a local, portable Wasm command and its JSON input.
// Bale does not interpret the input or grant the guest access to host files.
type Request = bale.PluginRequest

// OCIRequest selects a registry-hosted portable plugin and its JSON input.
// Ref may be a tag or digest; RunOCI reports the resolved immutable descriptor.
type OCIRequest struct {
	Ref   string
	Input json.RawMessage
}

// OCIResult contains a validated declaration and the resolved plugin root.
type OCIResult struct {
	Declaration bale.DependencyDeclaration
	Descriptor  v1.Descriptor
}

// Options controls guest resources and an optional trusted local native cache.
// Zero values select a 30-second execution timeout, 256 MiB of linear memory,
// and no disk cache. MemoryLimitBytes must be a multiple of 64 KiB, at most 4 GiB.
// It limits guest linear memory, not compiler or process memory.
type Options struct {
	// ExecutionTimeout starts after compilation. The caller's context also
	// covers waiting and execution, but compilation may delay cancellation.
	ExecutionTimeout time.Duration
	MemoryLimitBytes uint64
	// CacheDirectory must be private to the current user. Its contents are
	// executable, engine-specific machine code, never portable plugin artifacts.
	CacheDirectory string
}

// Runner adapts Run for a Bale client's configured-plugin execution callback.
// Resource and cache options remain private to the caller's execution policy.
func Runner(options Options) bale.PluginRunner {
	return func(ctx context.Context, request bale.PluginRequest) (bale.DependencyDeclaration, error) {
		return Run(ctx, request, options)
	}
}

// Pack writes a discovery plugin archive to caller-owned OCI storage and returns its
// deterministic manifest descriptor. The package bytes, including its notices,
// remain unchanged. Publication and tagging remain caller owned.
func Pack(ctx context.Context, store content.Storage, packagePath string) (v1.Descriptor, error) {
	return pluginoci.Pack(ctx, store, packagePath)
}

// RunOCI resolves a plugin through the caller's client and executes its verified
// portable module. Registry policy and credentials come from that client. The
// temporary module is removed before return; any failure discards the result.
func RunOCI(ctx context.Context, client *bale.Client, request OCIRequest, options Options) (result OCIResult, err error) {
	if client == nil {
		return OCIResult{}, errors.New("plugin client is required")
	}
	root, err := client.Resolve(ctx, request.Ref)
	if err != nil {
		return OCIResult{}, fmt.Errorf("resolve dependency plugin: %w", err)
	}
	module, err := pluginoci.Open(ctx, root.Store, root.Descriptor)
	if err != nil {
		return OCIResult{}, fmt.Errorf("open dependency plugin: %w", err)
	}
	defer func() {
		if closeErr := module.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary dependency plugin: %w", closeErr))
			result = OCIResult{}
		}
	}()
	declaration, err := Run(ctx, Request{ModulePath: module.Path, Input: request.Input}, options)
	if err != nil {
		return OCIResult{}, err
	}
	return OCIResult{Declaration: declaration, Descriptor: root.Descriptor}, nil
}

// Run executes a fresh guest and validates its dependency declaration. It does
// not resolve dependencies, read registry credentials, or publish attachments.
// Calls serialize process-wide to bound concurrent compilation costs. The guest
// receives no host environment, filesystem mounts or network capabilities.
// Any guest failure discards stdout, including failures after writing a result.
func Run(ctx context.Context, request Request, options Options) (bale.DependencyDeclaration, error) {
	raw, err := wasm.Run(ctx, wasm.Request{ModulePath: request.ModulePath, Input: request.Input}, wasm.Options{
		ExecutionTimeout: options.ExecutionTimeout,
		MemoryLimitBytes: options.MemoryLimitBytes,
		CacheDirectory:   options.CacheDirectory,
	})
	if err != nil {
		return bale.DependencyDeclaration{}, fmt.Errorf("run dependency plugin: %w", err)
	}
	declaration, err := bale.DecodeDependencyDeclaration(raw)
	if err != nil {
		return bale.DependencyDeclaration{}, fmt.Errorf("plugin response: %w", err)
	}
	return declaration, nil
}
