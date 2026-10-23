package acceptance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"go.yaml.in/yaml/v3"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

// runHelmConsumerAcceptance uses stock Helm's plugin installer and postrenderer
// contract. The source registry and all local bundle copies are already gone.
func runHelmConsumerAcceptance(t *testing.T, ctx context.Context, binary string, plugin bale.Root, chart bale.Root, records []bale.InspectedDestination, source, delivered []bale.Root) {
	t.Helper()
	helm := newHelmConsumerCLI(t, ctx, binary)
	helm.run(nil, "plugin", "install", "oci://"+plugin.Repository+":0.0.0", "--verify=false", "--plain-http")
	if notice, err := os.ReadFile(filepath.Join(helm.plugins, "bale", "LICENSE")); err != nil || len(notice) == 0 {
		t.Fatalf("installed plugin omitted its license: %v", err)
	}
	filter := helm
	filter.binary = filepath.Join(helm.plugins, "bale", "bale-relocate")
	filter.env = []string{"PATH=/nonexistent", "GOMAXPROCS=4"}
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	binding := chart.Repository + "@" + chart.Descriptor.Digest.String()
	filterArgs := []string{"--records-json", string(raw), "--chart", binding}
	values := []string{"--set-string", "image=" + source[0].Repository + ":stable", "--set-string", "helper=" + source[1].Repository + ":stable", "--set-string", "child.image=" + source[2].Repository + ":stable"}
	base := append([]string{"template", "relocated", "oci://" + binding, "--plain-http"}, values...)
	original := helm.run(nil, base...)
	if !bytes.Contains(original, []byte("helm.sh/hook: test")) {
		t.Fatal("Helm fixture omitted subchart hook")
	}
	// The installed CLI resolves its chart attachment and wires its packaged
	// private renderer; callers supply the chart once and no destination JSON.
	args := append([]string{"bale", "template", "relocated", "oci://" + binding, "--plain-http"}, values...)
	rendered := helm.run(nil, args...)
	verifyHelmConsumerOutput(t, original, rendered, source, delivered)
	// The installed native binary works without Helm, a shell or another program
	// on PATH, and accepts its own output without requiring source aliases again.
	direct := filterArgs
	filtered := filter.run(original, direct...)
	verifyHelmConsumerOutput(t, original, filtered, source, delivered)
	twice := filter.run(filtered, direct...)
	if !bytes.Equal(filtered, twice) {
		t.Fatal("native Helm filter is not idempotent")
	}
	// Explicit destination overrides need no chart attachment mapping. Their
	// spelling is preserved while the chart's remaining dependencies relocate.
	const override = "custom.example/manual/app:release"
	custom := bytes.ReplaceAll(original, []byte(source[0].Repository+":stable"), []byte(override))
	verifyOverride := func(output []byte) {
		t.Helper()
		if !bytes.Contains(output, []byte(override)) {
			t.Fatalf("custom destination override missing: %s", output)
		}
		verifyHelmConsumerOutput(t, custom, output, source[1:], delivered[1:])
	}
	verifyOverride(filter.run(custom, filterArgs...))
	customArgs := append(append([]string{}, args...), "--set-string", "image="+override)
	verifyOverride(helm.run(nil, customArgs...))
	// Invalid image syntax still fails atomically after a valid document.
	invalid := append(append([]byte{}, original...), []byte("\n---\napiVersion: v1\nkind: Pod\nspec: {containers: [{image: '@bad'}]}\n")...)
	filter.reject(invalid, "invalid image", filterArgs...)
	// A second delivery remains a distinct choice even if image locations happen
	// to match. Consumers must select provenance rather than choose by ordering.
	ambiguous := append([]bale.InspectedDestination{}, records...)
	second := records[0]
	second.Record.Bundle = "other.example/release@" + chart.Descriptor.Digest.String()
	ambiguous = append(ambiguous, second)
	raw, err = json.Marshal(ambiguous)
	if err != nil {
		t.Fatal(err)
	}
	filter.reject(original, "local destination record", "--records-json", string(raw), "--chart", binding)
	selected := filter.run(original, "--records-json", string(raw), "--chart", binding, "--bundle", records[0].Record.Bundle)
	verifyHelmConsumerOutput(t, original, selected, source, delivered)
	// This exercises install's real postrenderer path without a Kubernetes cluster
	// or claiming server-side admission, readiness or upgrade compatibility.
	install := append([]string{"bale", "install", "relocated", "oci://" + binding, "--plain-http", "--dry-run=client"}, values...)
	installed := helm.run(nil, install...)
	for _, root := range delivered {
		if !bytes.Contains(installed, []byte(root.Repository+"@"+root.Descriptor.Digest.String())) {
			t.Fatalf("client install omitted relocated image %s", root.Repository)
		}
	}
	for _, root := range source {
		if bytes.Contains(installed, []byte(root.Repository+":stable")) {
			t.Fatalf("client install retained source image %s", root.Repository)
		}
	}
	verifyHelmConsumerUnsupportedPlatform(t, helm, args)
	selectedArgs := append([]string{"bale", "--bundle", records[0].Record.Bundle, "template", "relocated", "oci://" + binding, "--plain-http"}, values...)
	verifyHelmConsumerOutput(t, original, helm.run(nil, selectedArgs...), source, delivered)
	t.Log("Helm consumer passed: one OCI-installed native CLI package, automatic attachment lookup and private renderer, pinned chart, main/init/hook relocation, idempotence, selection, atomic failures, unsupported platform and client dry-run install")
}

// Helm must reject a package for another architecture before launching its
// executable, even when the local file could otherwise run on this host.
func verifyHelmConsumerUnsupportedPlatform(t *testing.T, helm helmConsumerCLI, args []string) {
	t.Helper()
	path := filepath.Join(helm.plugins, "bale", "plugin.yaml")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	originalMode := info.Mode().Perm()
	if err := os.Chmod(path, originalMode|0200); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Errorf("restore installed plugin metadata: %v", err)
		}
		if err := os.Chmod(path, originalMode); err != nil {
			t.Errorf("restore installed plugin metadata permissions: %v", err)
		}
	}()
	var metadata map[string]any
	if err := yaml.Unmarshal(original, &metadata); err != nil {
		t.Fatal(err)
	}
	runtimeConfig, ok := metadata["runtimeConfig"].(map[string]any)
	if !ok {
		t.Fatal("installed plugin has no runtimeConfig")
	}
	commands, ok := runtimeConfig["platformCommand"].([]any)
	if !ok || len(commands) != 1 {
		t.Fatal("installed plugin must declare exactly one platform command")
	}
	command, ok := commands[0].(map[string]any)
	if !ok || command["os"] == "" || command["arch"] == "" || command["os"] == nil || command["arch"] == nil {
		t.Fatal("installed plugin must explicitly select its operating system and architecture")
	}
	command["arch"] = "bale-acceptance-unsupported-architecture"
	raw, err := yaml.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	helm.reject(nil, "no plugin command is applicable", args...)
}

// Packages are native Helm OCI plugins. Bale transfers their generic manifest
// graphs unchanged; no Bale-specific plugin packaging or runtime is involved.
func seedHelmConsumerPackage(t *testing.T, ctx context.Context, client *bale.Client, archive, host string) bale.Root {
	t.Helper()
	title := filepath.Base(archive)
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	store := memory.New()
	layer := content.NewDescriptorFromBytes(v1.MediaTypeImageLayerGzip, raw)
	layer.Annotations = map[string]string{v1.AnnotationTitle: title}
	if err := store.Push(ctx, layer, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	descriptor, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, "application/vnd.helm.plugin.v1+json", oras.PackManifestOptions{Layers: []v1.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	root := bale.Root{Repository: "fixture.example/tools/bale", Descriptor: descriptor, Store: store, Aliases: []string{"0.0.0"}, Referrers: bale.ReferrerPolicy{Include: "none"}}
	plan, err := client.Plan(ctx, bale.TransferRequest{Roots: []bale.Root{root}, Destination: host, RepositoryMap: map[string]string{root.Repository: "tools/bale"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Execute(ctx, plan); err != nil {
		t.Fatal(err)
	}
	root.Repository = host + "/tools/bale"
	return root
}

func newHelmConsumerCLI(t *testing.T, ctx context.Context, binary string) helmConsumerCLI {
	t.Helper()
	dir := t.TempDir()
	plugins := filepath.Join(dir, "data", "plugins")
	return helmConsumerCLI{t: t, ctx: ctx, binary: binary, plugins: plugins, env: []string{
		"PATH=/nonexistent", "GOMAXPROCS=4", "HOME=" + filepath.Join(dir, "home"),
		"HELM_DATA_HOME=" + filepath.Join(dir, "data"), "HELM_CONFIG_HOME=" + filepath.Join(dir, "config"),
		"HELM_CACHE_HOME=" + filepath.Join(dir, "cache"), "HELM_PLUGINS=" + plugins,
		"KUBECONFIG=" + filepath.Join(dir, "absent-kubeconfig"),
	}}
}

type helmConsumerCLI struct {
	t       *testing.T
	ctx     context.Context
	binary  string
	plugins string
	env     []string
}

func (h helmConsumerCLI) execute(input []byte, args ...string) ([]byte, []byte, error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.binary, args...)
	cmd.Env = h.env
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		h.t.Fatalf("consumer command timed out: %v\n%s", ctx.Err(), tail(stderr.Bytes(), 16000))
	}
	return stdout.Bytes(), stderr.Bytes(), err
}
func (h helmConsumerCLI) run(input []byte, args ...string) []byte {
	h.t.Helper()
	stdout, stderr, err := h.execute(input, args...)
	if err != nil {
		h.t.Fatalf("consumer %s: %v\nstdout:\n%s\nstderr:\n%s", args[0], err, tail(stdout, 16000), tail(stderr, 16000))
	}
	return stdout
}
func (h helmConsumerCLI) reject(input []byte, diagnostic string, args ...string) {
	h.t.Helper()
	stdout, stderr, err := h.execute(input, args...)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == 0 || len(stdout) != 0 || !strings.Contains(strings.ToLower(string(stderr)), diagnostic) {
		h.t.Fatalf("expected atomic consumer failure containing %q: %v\nstdout:\n%s\nstderr:\n%s", diagnostic, err, stdout, tail(stderr, 16000))
	}
}

func verifyHelmConsumerOutput(t *testing.T, original, rendered []byte, source, delivered []bale.Root) {
	t.Helper()
	expected := append([]byte{}, original...)
	for i, root := range source {
		expected = bytes.ReplaceAll(expected, []byte(root.Repository+":stable"), []byte(delivered[i].Repository+"@"+root.Descriptor.Digest.String()))
	}
	decode := func(raw []byte) []any {
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		var documents []any
		for {
			var document any
			err := decoder.Decode(&document)
			if errors.Is(err, io.EOF) {
				return documents
			}
			if err != nil {
				t.Fatalf("decode Helm manifests: %v", err)
			}
			if document != nil {
				documents = append(documents, document)
			}
		}
	}
	// Compare every field, including hook annotations and repeated/init images;
	// Helm owns comments and formatting around the postrendered documents.
	if !reflect.DeepEqual(decode(expected), decode(rendered)) {
		t.Fatalf("Helm relocation changed fields beyond recorded images\nexpected:\n%s\nactual:\n%s", expected, rendered)
	}
}
