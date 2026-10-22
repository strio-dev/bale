package wasm

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/wago-org/wago"
	"github.com/wago-org/wasi/p1"
)

type wagoEngine struct{}

func (wagoEngine) Run(ctx context.Context, request Request, options Options) (output []byte, err error) {
	cfg := wago.NewRuntimeConfig().
		WithMaxModuleBytes(ModuleLimit).
		WithMemoryLimitPages(uint32(options.MemoryLimitBytes/memoryPage)).
		WithMaxMemoriesPerModule(1).
		WithInstanceLimits(1, 0).
		WithBoundsChecks(wago.BoundsChecksExplicit).
		WithFunctionWorkers(1)
	runtime := wago.NewRuntime(wago.WithRuntimeConfig(cfg), wago.WithGuestArguments([]string{"bale-plugin"}))
	// Keep teardown inside the process-wide invocation permit. Close alone can
	// return while native resources are still being released. Cleanup must finish
	// even when the caller's execution context has already been cancelled.
	defer closeResource(&output, &err, "runtime", func() error { return runtime.CloseContext(context.Background()) })
	module, err := compileModule(ctx, runtime, request.ModulePath, options.CacheDirectory, cfg)
	if err != nil {
		return nil, fmt.Errorf("compile plugin: %w", err)
	}
	defer closeResource(&output, &err, "module", module.Close)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	execution, cancel := context.WithTimeout(ctx, options.ExecutionTimeout)
	defer cancel()
	stdout := boundedWriter{limit: OutputLimit}
	stderr := boundedWriter{limit: StderrLimit}
	imports := p1.Imports(p1.Config{
		Stdin: bytes.NewReader(request.Input), Stdout: &stdout, Stderr: &stderr,
		Args: []string{"bale-plugin"}, Context: execution,
		MaxOpenFiles: 3, MaxIOVecs: 1024, MaxSubscriptionsPerPoll: 1024,
	})
	instance, err := runtime.Instantiate(execution, module, wago.WithImports(imports))
	if err == nil {
		defer closeResource(&output, &err, "instance", instance.Close)
		_, err = instance.InvokeContext(execution, "_start")
	}
	if stdout.overflow {
		return nil, errors.New("plugin stdout exceeds 4 MiB")
	}
	if stderr.overflow {
		return nil, errors.New("plugin stderr exceeds 64 KiB")
	}
	if execution.Err() != nil {
		return nil, fmt.Errorf("execute plugin: %w", execution.Err())
	}
	var exit *wago.ExitError
	if errors.As(err, &exit) && exit.Code == 0 {
		err = nil
	}
	if err != nil {
		if len(stderr.data) != 0 {
			return nil, fmt.Errorf("execute plugin: %w; stderr: %s", err, stderr.data)
		}
		return nil, fmt.Errorf("execute plugin: %w", err)
	}
	return stdout.data, nil
}

func closeResource(output *[]byte, result *error, name string, close func() error) {
	if err := close(); err != nil {
		*result = errors.Join(*result, fmt.Errorf("close plugin %s: %w", name, err))
		*output = nil
	}
}

type boundedWriter struct {
	data     []byte
	limit    int
	overflow bool
}

func (writer *boundedWriter) Write(data []byte) (int, error) {
	if writer.overflow || len(data) > writer.limit-len(writer.data) {
		writer.overflow = true
		return 0, errors.New("plugin output limit exceeded")
	}
	writer.data = append(writer.data, data...)
	return len(data), nil
}
