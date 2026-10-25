package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/internal/bundle"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
)

// Move stable to a new manifest; optionally retain the original under legacy.
func cliDeliveryRefresh(t *testing.T, survivor bool) (string, v1.Descriptor, v1.Descriptor) {
	t.Helper()
	dir, _, _ := cliBundle(t)
	client, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := client.InspectLayout(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := oci.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := inspection.Metadata
	oldRoot, oldOrigin, oldObservation := m.Roots[0], m.Origins[0], m.Dependencies[0]
	raw, err := content.FetchAll(t.Context(), store, oldRoot.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Annotations = map[string]string{"example": "refreshed"}
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	member := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, raw)
	if err := store.Push(t.Context(), member, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	m.Roots[0].Descriptor, m.Origins[0].Descriptor, m.Dependencies[0].Subject = member, member, member
	if survivor {
		selection := m.Recipe.Include[0]
		selection.Ref = "unreachable.invalid/app:legacy"
		m.Recipe.Include = append(m.Recipe.Include, selection)
		oldRoot.Input, oldRoot.Reference = 1, "legacy"
		m.Roots = append(m.Roots, oldRoot)
		m.Origins = append(m.Origins, oldOrigin)
		m.Dependencies = append(m.Dependencies, oldObservation)
	}
	snapshot, err := bundle.BuildSnapshot(t.Context(), store, m)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{snapshot.Descriptor}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, snapshot.Descriptor, member
}

func cliEditedDelivery(t *testing.T, dir string) (string, bale.DeliveryFile) {
	t.Helper()
	client, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	file, err := client.ExportDeliveryLayout(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	file.Artifacts[0].Path = "images/custom"
	path := filepath.Join(t.TempDir(), "old.delivery.yaml")
	writeDeliveryFixture(t, path, file)
	return path, file
}

func writeDeliveryFixture(t *testing.T, path string, file bale.DeliveryFile) {
	t.Helper()
	raw, err := bale.EncodeDeliveryFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryUpdateCLIFormatsAndReadOnlyInputs(t *testing.T) {
	before, _, _ := cliBundle(t)
	after, root, member := cliDeliveryRefresh(t, false)
	input, file := cliEditedDelivery(t, before)
	beforeFiles, afterFiles := layoutBytes(t, before), layoutBytes(t, after)
	docker := t.TempDir()
	if err := os.WriteFile(filepath.Join(docker, "config.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", docker)
	for _, tc := range []struct {
		name, format string
	}{
		{"readable", ""}, {"JSON", "json"}, {"JSON shortcut", "{{json}}"},
		{"JSON helper", "{{json .}}"}, {"template", "{{.Delivery.Bundle}}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "updated.delivery.yaml")
			args := []string{"delivery", "update", before, after, "--layout", "-f" + input, "-o" + output}
			if tc.format != "" {
				args = append(args, "--format", tc.format)
			}
			var out, errs bytes.Buffer
			if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
				t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
			}
			raw, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			updated, err := bale.DecodeDeliveryFile(raw)
			if err != nil || updated.Bundle != root.Digest || len(updated.Artifacts) != 1 || updated.Artifacts[0].Path != "images/custom" || updated.Artifacts[0].Ref != "unreachable.invalid/app@"+member.Digest.String() {
				t.Fatalf("lost paths or binding: %s, %v", raw, err)
			}
			switch tc.name {
			case "readable":
				if !strings.Contains(out.String(), "moved") || !strings.Contains(out.String(), "images/custom") || !strings.Contains(out.String(), output) {
					t.Fatalf("missing update report: %s", &out)
				}
			case "template":
				if out.String() != root.Digest.String()+"\n" {
					t.Fatalf("template output: %s", &out)
				}
			default:
				var result bale.DeliveryUpdateResult
				if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Delivery == nil || !reflect.DeepEqual(*result.Delivery, updated) || len(result.Changes) != 1 || result.Changes[0].Change != "moved" || result.Conflicts == nil {
					t.Fatalf("invalid report: %s, %v", &out, err)
				}
			}
		})
	}
	if !reflect.DeepEqual(beforeFiles, layoutBytes(t, before)) || !reflect.DeepEqual(afterFiles, layoutBytes(t, after)) {
		t.Fatal("update changed a layout")
	}
	raw, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := bale.DecodeDeliveryFile(raw)
	if err != nil || !reflect.DeepEqual(actual, file) {
		t.Fatalf("input file changed: %s, %v", raw, err)
	}
}

func TestDeliveryUpdateStdin(t *testing.T) {
	before, _, _ := cliBundle(t)
	after, root, _ := cliDeliveryRefresh(t, false)
	input, _ := cliEditedDelivery(t, before)
	raw, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	withInput(t, string(raw))
	output := filepath.Join(t.TempDir(), "updated.delivery.yaml")
	var out, errs bytes.Buffer
	args := []string{"delivery", "update", "--layout", before, after, "-f", "-", "-o", output}
	if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
		t.Fatalf("stdin update: exit %d stdout %s stderr %s", code, &out, &errs)
	}
	raw, err = os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if file, err := bale.DecodeDeliveryFile(raw); err != nil || file.Bundle != root.Digest || len(file.Artifacts) != 1 || file.Artifacts[0].Path != "images/custom" {
		t.Fatalf("updated delivery: %s: %v", raw, err)
	}
}

func TestDeliveryUpdateConflictReportsWithoutFile(t *testing.T) {
	before, _, _ := cliBundle(t)
	after, _, _ := cliDeliveryRefresh(t, true)
	input, _ := cliEditedDelivery(t, before)
	for _, format := range []string{"", "json"} {
		outputDir := t.TempDir()
		output := filepath.Join(outputDir, "updated.yaml")
		args := []string{"delivery", "update", "--layout", before, after, "-f", input, "-o", output}
		if format != "" {
			args = append(args, "--format", format)
		}
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 125 || !strings.HasPrefix(errs.String(), "Error: ") {
			t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
		}
		if format == "json" {
			var result bale.DeliveryUpdateResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Delivery != nil || len(result.Conflicts) == 0 {
				t.Fatalf("missing conflict evidence: %s, %v", &out, err)
			}
		} else if !strings.Contains(out.String(), "no delivery file written") || !strings.Contains(out.String(), "images/custom") {
			t.Fatalf("missing readable conflicts: %s", &out)
		}
		entries, err := os.ReadDir(outputDir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("conflict left output files: %v, %v", entries, err)
		}
	}
}

func TestDeliveryUpdateProtectsOutputsAndCleansFailures(t *testing.T) {
	before, _, _ := cliBundle(t)
	after, _, _ := cliDeliveryRefresh(t, false)
	input, file := cliEditedDelivery(t, before)
	for _, name := range []string{"existing", "same as input", "missing before", "missing after", "stale binding", "unknown identity", "invalid template", "template execution", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			outputDir := t.TempDir()
			output := filepath.Join(outputDir, "updated.yaml")
			old, next, source := before, after, input
			ctx := t.Context()
			var flags []string
			switch name {
			case "existing":
				if err := os.WriteFile(output, []byte("existing edits"), 0600); err != nil {
					t.Fatal(err)
				}
			case "same as input":
				output = input
			case "missing before":
				old = filepath.Join(t.TempDir(), "missing")
			case "missing after":
				next = filepath.Join(t.TempDir(), "missing")
			case "stale binding", "unknown identity":
				copy := file
				copy.Artifacts = append([]bale.DeliveryArtifact{}, file.Artifacts...)
				if name == "stale binding" {
					copy.Bundle = previewDescriptor("stale").Digest
				} else {
					copy.Artifacts[0].Ref = "unknown.invalid/app@" + previewDescriptor("unknown").Digest.String()
				}
				source = filepath.Join(t.TempDir(), "invalid.yaml")
				writeDeliveryFixture(t, source, copy)
			case "invalid template":
				flags = []string{"--format", "{{"}
			case "template execution":
				flags = []string{"--format", "prefix{{.Missing}}"}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			args := append([]string{"delivery", "update", "--layout", old, next, "-f", source, "-o", output}, flags...)
			var out, errs bytes.Buffer
			if code := runContext(ctx, args, &out, &errs); code != 125 || out.Len() != 0 || !strings.HasPrefix(errs.String(), "Error: ") {
				t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
			}
			entries, err := os.ReadDir(outputDir)
			if err != nil || (name != "existing" && len(entries) != 0) {
				t.Fatalf("failure left temporary files: %v, %v", entries, err)
			}
			if name == "existing" {
				raw, err := os.ReadFile(output)
				if err != nil || string(raw) != "existing edits" {
					t.Fatalf("existing output changed: %s, %v", raw, err)
				}
			}
			if name == "same as input" {
				raw, err := os.ReadFile(input)
				if err != nil {
					t.Fatal(err)
				}
				got, err := bale.DecodeDeliveryFile(raw)
				if err != nil || !reflect.DeepEqual(got, file) {
					t.Fatalf("original edits changed: %s, %v", raw, err)
				}
			}
		})
	}
}

func TestDeliveryUpdateOptionsAndHelp(t *testing.T) {
	for _, args := range [][]string{{"delivery"}, {"help", "delivery"}, {"delivery", "--help"}} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 || !strings.Contains(out.String(), "bale delivery [command]") {
			t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
		}
	}
	for _, args := range [][]string{{"delivery", "update", "--help"}, {"help", "delivery", "update"}} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 || !strings.Contains(out.String(), "bale delivery update [options] BEFORE AFTER") {
			t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"delivery", "unknown"}, "unsupported command"},
		{[]string{"delivery", "update"}, "required"},
		{[]string{"delivery", "update", "old", "new", "--layout", "-f", "file"}, "required"},
		{[]string{"delivery", "update", "old", "new", "--layout", "-f", "file", "-o="}, "nonempty path"},
		{[]string{"delivery", "update", "old", "new", "--layout", "-f", "file", "--file", "other", "-o", "out"}, "duplicate option"},
		{[]string{"delivery", "update", "old", "new", "--layout", "-f", "file", "-o", "out", "--output", "other"}, "duplicate option"},
		{[]string{"delivery", "update", "old", "new", "--layout", "-f", "file", "-o", "out", "--format", "{{"}, "invalid --format"},
		{[]string{"delivery", "update", "--registry", "team/b:tag", "example.invalid/b:tag", "-f", "file", "-o", "out"}, "explicit registry"},
		{[]string{"delivery", "update", "--registry", "example.invalid/b:tag", "example.invalid/b", "-f", "file", "-o", "out"}, "explicit tag"},
	} {
		var out, errs bytes.Buffer
		if code := run(tc.args, &out, &errs); code != 125 || out.Len() != 0 || !strings.Contains(errs.String(), tc.want) {
			t.Fatalf("%v: exit %d stdout %s stderr %s", tc.args, code, &out, &errs)
		}
	}
}
