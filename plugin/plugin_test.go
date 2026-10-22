package plugin_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/internal/wasmtest"
	"github.com/strio-dev/bale/plugin"
)

func moduleFile(t *testing.T, module []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "producer.wasm")
	if err := os.WriteFile(path, module, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunValidatesDependencyProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		exit         uint32
		wantCount    int
		valid        bool
	}{
		{"deduplicated", `{"schemaVersion":1,"context":"example","dependencies":[{"ref":"example.com/app:v1"},{"ref":"example.com/app:v1"}]}`, 0, 1, true},
		{"empty", `{"schemaVersion":1,"dependencies":[]}`, 0, 0, true},
		{"unknown version", `{"schemaVersion":2,"dependencies":[]}`, 0, 0, false},
		{"missing version", `{"dependencies":[]}`, 0, 0, false},
		{"proof envelope", `{"schemaVersion":1,"files":{},"declaration":{"schemaVersion":1,"dependencies":[]}}`, 0, 0, false},
		{"implicit registry", `{"schemaVersion":1,"dependencies":[{"ref":"app:v1"}]}`, 0, 0, false},
		{"duplicate field", `{"schemaVersion":1,"dependencies":[],"dependencies":[]}`, 0, 0, false},
		{"failed after result", `{"schemaVersion":1,"dependencies":[]}`, 7, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := plugin.Request{ModulePath: moduleFile(t, wasmtest.Command(tc.output, tc.exit)), Input: []byte(`{}`)}
			declaration, err := plugin.Run(context.Background(), request, plugin.Options{})
			if !tc.valid {
				if err == nil || declaration.SchemaVersion != 0 || declaration.Dependencies != nil {
					t.Fatalf("failed result must be empty: %+v, %v", declaration, err)
				}
				return
			}
			if err != nil || declaration.SchemaVersion != bale.DependencyVersion || len(declaration.Dependencies) != tc.wantCount {
				t.Fatalf("declaration %+v: %v", declaration, err)
			}
		})
	}
}

func TestRunPreservesCancellation(t *testing.T) {
	_, err := plugin.Run(context.Background(), plugin.Request{
		ModulePath: moduleFile(t, wasmtest.Loop()), Input: []byte(`{}`),
	}, plugin.Options{ExecutionTimeout: 10 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
}
