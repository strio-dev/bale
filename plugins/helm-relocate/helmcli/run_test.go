package helmcli

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

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	bale "github.com/strio-dev/bale"
	relocate "github.com/strio-dev/bale/plugins/helm-relocate"
)

func selected(t *testing.T) relocate.Selection {
	t.Helper()
	d := digest.FromString("chart")
	chart := "registry.example/app@" + d.String()
	records := []bale.InspectedDestination{{
		Repository: "registry.example/app",
		Subject:    v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: d, Size: 100},
		Descriptor: v1.Descriptor{MediaType: v1.MediaTypeImageManifest, ArtifactType: "application/vnd.bale.destination.v1+json", Digest: digest.FromString("attachment"), Size: 100},
		Local:      true,
		Record: bale.DestinationRecord{
			SchemaVersion: 1,
			Bundle:        "registry.example/bundle@" + digest.FromString("bundle").String(),
			Source:        "source.example/app@" + d.String(), Destination: chart,
			Resolutions: []bale.DestinationResolution{{
				Capture: digest.FromString("capture"), Outcome: "selected",
				Declaration:  &bale.DestinationDeclaration{Digest: digest.FromString("declaration"), Context: "prod"},
				Dependencies: []bale.DestinationDependency{},
			}},
		},
	}}
	raw, err := bale.EncodeDestinationRecord(records[0].Record)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &records[0].Record); err != nil {
		t.Fatal(err)
	}
	s, err := relocate.Select(records, relocate.Options{Chart: chart})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func setup(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	plugin := filepath.Join(dir, "postrenderers", "bale-postrenderer")
	if err := os.MkdirAll(plugin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "plugin.yaml"), []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	// Execution is injected below; use a real independent file without assuming
	// host programs exist at FHS paths inside the Nix build sandbox.
	binary := filepath.Join(dir, "helm")
	if err := os.WriteFile(binary, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	return []string{"HELM_BIN=" + binary, "HELM_PLUGIN_DIR=" + dir, "HELM_PLUGINS=/existing/plugins", "HELM_REGISTRY_CONFIG=/credentials", "HELM_NAMESPACE=test", "KUBECONFIG=/kubeconfig", "SECRET=value"}
}
func TestRunPinsAndPassesThrough(t *testing.T) {
	env := setup(t)
	selection := selected(t)
	calls := 0
	recordsPath := ""
	input := strings.NewReader("input")
	var out, stderr bytes.Buffer
	err := run(context.Background(), []string{"template", "release", "oci://registry.example/app", "--version", "1.2.3", "-f", "values.yaml", "--namespace=test"}, Streams{input, &out, &stderr}, env, func(_ context.Context, p invocation) (relocate.Selection, error) {
		calls++
		if p.chart != "registry.example/app:1.2.3" || p.registry.RegistryConfig != "/credentials" {
			t.Fatalf("%+v", p)
		}
		return selection, nil
	}, func(_ context.Context, binary string, args, childEnv []string, streams Streams) error {
		if binary != envValue(env, "HELM_BIN") || streams.In != input || streams.Out != &out || streams.Err != &stderr {
			t.Fatal("changed process streams")
		}
		want := []string{"template", "release", "oci://" + selection.Chart, "-f", "values.yaml", "--namespace=test", "--post-renderer", "bale-postrenderer"}
		if !reflect.DeepEqual(args[:len(want)], want) {
			t.Fatalf("%q", args)
		}
		for i, arg := range args {
			if arg == "--post-renderer-args=--records-file" {
				recordsPath = strings.TrimPrefix(args[i+1], "--post-renderer-args=")
			}
		}
		info, err := os.Stat(recordsPath)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("file permission: %v %v", info, err)
		}
		raw, err := os.ReadFile(recordsPath)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := selection.MarshalRecords()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, expected) {
			t.Fatal("wrong records")
		}
		wantPath := filepath.Join(envValue(env, "HELM_PLUGIN_DIR"), "postrenderers") + string(os.PathListSeparator) + "/existing/plugins"
		if envValue(childEnv, "HELM_PLUGINS") != wantPath || envValue(childEnv, "SECRET") != "value" || envValue(childEnv, "KUBECONFIG") != "/kubeconfig" {
			t.Fatalf("env %q", childEnv)
		}
		return &ExitError{Code: 19, Err: errors.New("child failure")}
	})
	var child *ExitError
	if !errors.As(err, &child) || child.Code != 19 || calls != 1 {
		t.Fatalf("%v calls %d", err, calls)
	}
	if _, err := os.Stat(recordsPath); !os.IsNotExist(err) {
		t.Fatal("records file leaked")
	}
}
func TestSelectionFailureDoesNotRunHelm(t *testing.T) {
	err := run(context.Background(), []string{"install", "release", "oci://registry.example/app:1"}, Streams{}, setup(t), func(context.Context, invocation) (relocate.Selection, error) {
		return relocate.Selection{}, errors.New("ambiguous")
	}, func(context.Context, string, []string, []string, Streams) error { t.Fatal("executed Helm"); return nil })
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("%v", err)
	}
}
func TestHelpDoesNotResolve(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, Streams{Out: &out}, nil, nil, nil); err != nil || !strings.Contains(out.String(), "helm bale") {
		t.Fatalf("%s %v", out.String(), err)
	}
}
func TestSelfRecursionRejected(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "helm")
	if err := os.Symlink(self, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := helmExecutable(alias, nil); err == nil {
		t.Fatal("self recursion accepted")
	}
}
