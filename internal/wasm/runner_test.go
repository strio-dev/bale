package wasm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/strio-dev/bale/internal/wasmtest"
)

func moduleFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "command.wasm")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommand(t *testing.T) {
	for _, tc := range []struct {
		name, output, wantError string
		exit                    uint32
	}{
		{name: "unicode JSON", output: `{"message":"構成"}`},
		{name: "nonzero discards valid output", output: `{}`, exit: 7, wantError: "7"},
		{name: "invalid JSON", output: `invalid`, wantError: "JSON"},
		{name: "invalid UTF8", output: "\"\xff\"", wantError: "UTF-8"},
		{name: "ignored stdout overflow", output: strings.Repeat("x", OutputLimit+1), wantError: "stdout exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Run(context.Background(), Request{moduleFile(t, wasmtest.Command(tc.output, tc.exit)), []byte(`{}`)}, Options{})
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || result != nil {
					t.Fatalf("result=%q error=%v", result, err)
				}
			} else if err != nil || string(result) != tc.output {
				t.Fatalf("result=%q error=%v", result, err)
			}
		})
	}
}

func TestExecutionCancellation(t *testing.T) {
	path := moduleFile(t, wasmtest.Loop())
	start := time.Now()
	result, err := Run(context.Background(), Request{path, []byte(`{}`)}, Options{ExecutionTimeout: 20 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) || result != nil {
		t.Fatalf("result=%q error=%v", result, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("native loop did not stop promptly")
	}
}

func TestStderrLimitAndError(t *testing.T) {
	for _, tc := range []struct {
		output string
		exit   uint32
		want   string
	}{
		{"bounded diagnostic", 2, "stderr: bounded diagnostic"},
		{strings.Repeat("x", StderrLimit+1), 0, "stderr exceeds 64 KiB"},
	} {
		path := moduleFile(t, wasmtest.Stderr(tc.output, tc.exit))
		result, err := Run(context.Background(), Request{path, []byte(`{}`)}, Options{})
		if err == nil || !strings.Contains(err.Error(), tc.want) || result != nil {
			t.Fatalf("result=%q error=%v", result, err)
		}
	}
}

func TestTrapDiscardsOutput(t *testing.T) {
	module := wasmtest.Command(`{"discard":true}`, 0)
	// Replace the proc_exit call with unreachable + nop, keeping body lengths.
	position := bytes.Index(module, []byte{0x10, 1, 0x0b})
	if position < 0 {
		t.Fatal("fixture has no exit call")
	}
	module[position], module[position+1] = 0, 1
	result, err := Run(context.Background(), Request{moduleFile(t, module), []byte(`{}`)}, Options{})
	if err == nil || result != nil {
		t.Fatalf("trapped guest returned output: %q %v", result, err)
	}
}

func TestMemoryLimit(t *testing.T) {
	path := moduleFile(t, wasmtest.Command(strings.Repeat("x", memoryPage), 0))
	if out, err := Run(context.Background(), Request{path, []byte(`{}`)}, Options{MemoryLimitBytes: memoryPage}); err == nil || out != nil {
		t.Fatalf("two-page initial memory accepted: %q %v", out, err)
	}
}

func TestSecondLanguageWASI(t *testing.T) {
	input, err := os.ReadFile("testdata/request.json")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := Run(context.Background(), Request{"testdata/rust-producer.wasm", input}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile("testdata/expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err = json.Unmarshal(actual, &got); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(expected, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %s want %s", actual, expected)
	}
	output, err := Run(context.Background(), Request{"testdata/rust-producer.wasm", []byte(`{}`)}, Options{})
	if err == nil || output != nil || !strings.Contains(err.Error(), "stderr:") {
		t.Fatalf("bad request result=%q error=%v", output, err)
	}
}

type engineFunc func(context.Context, Request, Options) ([]byte, error)

func (fn engineFunc) Run(ctx context.Context, req Request, opts Options) ([]byte, error) {
	return fn(ctx, req, opts)
}

func TestNeutralBoundary(t *testing.T) {
	request := Request{moduleFile(t, []byte("placeholder")), []byte(`{}`)}
	called := false
	fake := engineFunc(func(_ context.Context, _ Request, opts Options) ([]byte, error) {
		called = true
		if opts.MemoryLimitBytes != DefaultMemoryLimit || opts.ExecutionTimeout != DefaultExecutionTimeout {
			t.Fatalf("unexpected defaults: %+v", opts)
		}
		return []byte(`{"engine":"replacement"}`), nil
	})
	output, err := run(context.Background(), request, Options{}, fake)
	if err != nil || !called || !bytes.Contains(output, []byte("replacement")) {
		t.Fatalf("result=%q error=%v", output, err)
	}
	for _, opts := range []Options{{ExecutionTimeout: -1}, {MemoryLimitBytes: 1}, {MemoryLimitBytes: (1 << 32) + memoryPage}} {
		called = false
		if _, err := run(context.Background(), request, opts, fake); err == nil || called {
			t.Fatal("invalid options reached engine")
		}
	}
	for _, input := range [][]byte{nil, []byte(`{} {}`), []byte("\"\xff\""), bytes.Repeat([]byte(" "), InputLimit+1)} {
		called = false
		bad := request
		bad.Input = input
		if _, err := run(context.Background(), bad, Options{}, fake); err == nil || called {
			t.Fatal("invalid input reached engine")
		}
	}
}

func TestInvocationWaitCancellation(t *testing.T) {
	request := Request{moduleFile(t, []byte("placeholder")), []byte(`{}`)}
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	fake := engineFunc(func(context.Context, Request, Options) ([]byte, error) {
		close(started)
		<-release
		return []byte(`{}`), nil
	})
	go func() { _, err := run(context.Background(), request, Options{}, fake); done <- err }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := run(ctx, request, Options{}, engineFunc(func(context.Context, Request, Options) ([]byte, error) {
		t.Error("second engine ran while first was active")
		return nil, nil
	}))
	close(release)
	if first := <-done; first != nil {
		t.Fatal(first)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting error=%v", err)
	}
}

func TestStickyWriter(t *testing.T) {
	writer := boundedWriter{limit: 3}
	if _, err := writer.Write([]byte("abcd")); err == nil {
		t.Fatal("overflow accepted")
	}
	if _, err := writer.Write([]byte("{}")); err == nil || !writer.overflow || len(writer.data) != 0 {
		t.Fatal("overflow was not sticky")
	}
}

func TestCleanupHoldsPermitAndDiscardsOutputOnFailure(t *testing.T) {
	request := Request{moduleFile(t, []byte("placeholder")), []byte(`{}`)}
	cleaning := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	cleanupErr := errors.New("cleanup failed")
	engine := engineFunc(func(context.Context, Request, Options) (output []byte, err error) {
		defer closeResource(&output, &err, "runtime", func() error {
			close(cleaning)
			<-release
			return cleanupErr
		})
		return []byte(`{}`), nil
	})
	go func() {
		output, err := run(context.Background(), request, Options{}, engine)
		if output != nil {
			t.Error("cleanup failure returned successful output")
		}
		done <- err
	}()
	<-cleaning
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, waiting := run(ctx, request, Options{}, engineFunc(func(context.Context, Request, Options) ([]byte, error) {
		t.Error("invocation began while previous runtime was closing")
		return nil, nil
	}))
	close(release)
	if err := <-done; !errors.Is(err, cleanupErr) {
		t.Fatalf("lost cleanup error: %v", err)
	}
	if !errors.Is(waiting, context.DeadlineExceeded) {
		t.Fatalf("permit released before teardown: %v", waiting)
	}
}

func TestCleanupPreservesPrimaryError(t *testing.T) {
	primary, closing := errors.New("guest failed"), errors.New("close failed")
	output, result := []byte(`{}`), primary
	closeResource(&output, &result, "module", func() error { return closing })
	if output != nil || !errors.Is(result, primary) || !errors.Is(result, closing) {
		t.Fatalf("output=%s error=%v", output, result)
	}
}
