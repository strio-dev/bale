// Package wasm executes portable WASI command modules behind an internal engine boundary.
package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
	"unicode/utf8"
)

const (
	ModuleLimit             = 128 << 20
	InputLimit              = 8 << 20
	OutputLimit             = 4 << 20
	StderrLimit             = 64 << 10
	DefaultMemoryLimit      = 256 << 20
	DefaultExecutionTimeout = 30 * time.Second
	memoryPage              = 64 << 10
)

type Request struct {
	ModulePath string
	Input      []byte
}

type Options struct {
	ExecutionTimeout time.Duration
	MemoryLimitBytes uint64
	CacheDirectory   string
}

// engine is the only boundary a replacement WASI implementation must satisfy.
// Its output is untrusted until the runner validates it.
type engine interface {
	Run(context.Context, Request, Options) ([]byte, error)
}

// invocationPermit bounds compiler and execution memory across all callers.
var invocationPermit = make(chan struct{}, 1)

// Run executes one WASI command. ExecutionTimeout starts after compilation.
// The current engine cannot interrupt compilation; context cancellation is
// checked before and after it, without leaving detached compiler goroutines.
func Run(ctx context.Context, request Request, options Options) ([]byte, error) {
	return run(ctx, request, options, wagoEngine{})
}

func run(ctx context.Context, request Request, options Options, runtime engine) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(request.Input) > InputLimit {
		return nil, fmt.Errorf("plugin input exceeds %d bytes", InputLimit)
	}
	if !utf8.Valid(request.Input) || !json.Valid(request.Input) {
		return nil, errors.New("plugin input must be valid UTF-8 JSON")
	}
	if options.ExecutionTimeout < 0 {
		return nil, errors.New("plugin execution timeout must be positive")
	}
	if options.ExecutionTimeout == 0 {
		options.ExecutionTimeout = DefaultExecutionTimeout
	}
	if options.MemoryLimitBytes == 0 {
		options.MemoryLimitBytes = DefaultMemoryLimit
	}
	if options.MemoryLimitBytes > 1<<32 || options.MemoryLimitBytes%memoryPage != 0 {
		return nil, errors.New("plugin memory limit must be a multiple of 65536 bytes, at most 4 GiB")
	}
	stat, err := os.Stat(request.ModulePath)
	if err != nil {
		return nil, fmt.Errorf("plugin module: %w", err)
	}
	if !stat.Mode().IsRegular() || stat.Size() < 0 || stat.Size() > ModuleLimit {
		return nil, errors.New("plugin module must be a regular file at most 128 MiB")
	}
	select {
	case invocationPermit <- struct{}{}:
		defer func() { <-invocationPermit }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	output, err := runtime.Run(ctx, request, options)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(output) > OutputLimit {
		return nil, errors.New("plugin stdout exceeds 4 MiB")
	}
	if !utf8.Valid(output) || !json.Valid(output) {
		return nil, errors.New("plugin stdout must be valid UTF-8 JSON")
	}
	return output, nil
}
