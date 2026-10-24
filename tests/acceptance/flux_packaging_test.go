package acceptance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"go.yaml.in/yaml/v3"
	"oras.land/oras-go/v2/content/oci"
)

// TestFluxPackagingRegistryAcceptance bootstraps the shipped Flux adapter using
// its real image and chart, without a container runtime or a live source registry.
func TestFluxPackagingRegistryAcceptance(t *testing.T) {
	if os.Getenv("BALE_ACCEPTANCE_FLUX_IMAGE") == "" && os.Getenv("BALE_ACCEPTANCE_FLUX_CHART") == "" {
		t.Skip("run registry acceptance with the packaged Flux service image and chart")
	}
	names := []string{"BALE_ACCEPTANCE_BINARY", "BALE_ACCEPTANCE_DISTRIBUTION", "BALE_ACCEPTANCE_ZOT", "BALE_ACCEPTANCE_HELM_PLUGIN", "BALE_ACCEPTANCE_HELM", "BALE_ACCEPTANCE_HELM_CONSUMER_ARCHIVE", "BALE_ACCEPTANCE_FLUX_IMAGE", "BALE_ACCEPTANCE_FLUX_CHART"}
	paths := make([]string, len(names))
	for i, name := range names {
		paths[i] = os.Getenv(name)
		info, err := os.Stat(paths[i])
		directory := i == 6
		if err != nil || !filepath.IsAbs(paths[i]) || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) || ((i < 3 || i == 4) && info.Mode()&0111 == 0) {
			t.Fatalf("%s must name an absolute packaged %s: %q (%v)", name, map[bool]string{true: "directory", false: "file"}[directory], paths[i], err)
		}
	}
	for _, backend := range []struct{ name, binary string }{{"distribution", paths[1]}, {"zot", paths[2]}} {
		t.Run(backend.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			runFluxPackagingCycle(t, ctx, paths, backend.name, backend.binary)
		})
	}
}

func runFluxPackagingCycle(t *testing.T, ctx context.Context, paths []string, kind, backend string) {
	t.Helper()
	source := startRegistry(t, ctx, kind, backend, "Flux package source")
	destination := startRegistry(t, ctx, kind, backend, "Flux package destination")
	dir := t.TempDir()
	command := cli{t: t, ctx: ctx, binary: paths[0], hosts: []string{source.host, destination.host}, env: []string{"PATH=/nonexistent", "DOCKER_CONFIG=" + filepath.Join(dir, "docker"), "XDG_CACHE_HOME=" + filepath.Join(dir, "cache"), "XDG_DATA_HOME=" + filepath.Join(dir, "data")}}
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: command.hosts})
	if err != nil {
		t.Fatal(err)
	}
	image := publishFluxPackageImage(t, ctx, client, paths[6], source.host)
	helm := newHelmConsumerCLI(t, ctx, paths[4])
	var chartMetadata struct{ Name, Version string }
	if err := yaml.Unmarshal(helm.run(nil, "show", "chart", paths[7]), &chartMetadata); err != nil || chartMetadata.Name == "" || chartMetadata.Version == "" {
		t.Fatalf("packaged chart metadata: %+v (%v)", chartMetadata, err)
	}
	helm.run(nil, "push", paths[7], "oci://"+source.host+"/charts", "--plain-http")
	chart, err := client.Resolve(ctx, source.host+"/charts/"+chartMetadata.Name+":"+chartMetadata.Version)
	if err != nil {
		t.Fatal(err)
	}
	consumer := seedHelmConsumerPackage(t, ctx, client, paths[5], source.host)
	plugin := source.host + "/plugins/helm:v1"
	command.run("publish discovery plugin", "plugins", "push", "-f", paths[3], plugin)
	values := map[string]any{
		"image":    map[string]string{"repository": image.Repository, "tag": "proof"},
		"releases": []any{map[string]string{"name": "web", "chart": "registry.invalid/charts/app:v1"}},
	}
	valueJSON, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	valuePath := filepath.Join(dir, "values.json")
	writeFile(t, valuePath, valueJSON)
	recipe := map[string]any{"include": []any{
		map[string]any{"ref": chart.Repository + ":" + chartMetadata.Version, "plugin": map[string]any{
			"ref":   plugin,
			"input": map[string]any{"schemaVersion": 1, "release": "adapter", "namespace": "default", "context": "bootstrap", "values": values},
			"blobs": map[string]any{"chart": map[string]string{"mediaType": "application/vnd.cncf.helm.chart.content.v1.tar+gzip"}},
		}},
		map[string]any{"ref": consumer.Repository + ":0.0.0"},
	}}
	raw, err := json.Marshal(recipe)
	if err != nil {
		t.Fatal(err)
	}
	recipePath, layout, deliveryPath := filepath.Join(dir, "bale.yaml"), filepath.Join(dir, "capture"), filepath.Join(dir, "delivery.yaml")
	writeFile(t, recipePath, raw)
	created := decode[bale.CreateResult](t, command.run("discover adapter image from packaged chart", "create", "--layout", "-f", recipePath, layout, "--output-delivery", deliveryPath, "--format", "json"))
	if len(created.Metadata.Plugins) != 1 {
		t.Fatalf("expected one discovery plugin: %+v", created.Metadata.Plugins)
	}
	selected := 0
	for _, observation := range created.Metadata.Dependencies {
		if observation.Repository != chart.Repository || observation.Subject.Digest != chart.Descriptor.Digest || observation.Outcome != "selected" {
			continue
		}
		selected++
		if observation.Declaration == nil || observation.Declaration.Digest != created.Metadata.Plugins[0].Declaration.Digest || len(observation.Dependencies) != 1 {
			t.Fatalf("expected one discovered service image in the chart declaration: %+v", observation)
		}
		dependency := observation.Dependencies[0]
		if dependency.Ref != image.Repository+":proof" || dependency.Descriptor.Digest != image.Descriptor.Digest {
			t.Fatalf("discovery did not lock the packaged service image: %+v", dependency)
		}
	}
	if selected != 1 {
		t.Fatalf("expected one selected chart declaration, got %d", selected)
	}
	verifyGraph(t, ctx, image.Store, oci.NewStorageFromFS(os.DirFS(layout)), image.Descriptor)
	source.stop(t)
	assertStopped(t, ctx, source)
	raw, err = os.ReadFile(deliveryPath)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := bale.DecodeDeliveryFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i := range delivery.Artifacts {
		row := &delivery.Artifacts[i]
		switch row.Ref {
		case chart.Repository + "@" + chart.Descriptor.Digest.String():
			row.Path = "bootstrap/charts/adapter"
		case image.Repository + "@" + image.Descriptor.Digest.String():
			row.Path = "bootstrap/images/adapter"
		case consumer.Repository + "@" + consumer.Descriptor.Digest.String():
			row.Path = "bootstrap/tools/bale"
		}
	}
	raw, err = bale.EncodeDeliveryFile(delivery)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, deliveryPath, raw)
	pushed := decode[bale.PushResult](t, command.run("deliver adapter with source offline", "push", "--layout", layout, destination.host+"/release:bootstrap", "--delivery", deliveryPath, "--format", "json"))
	if pushed.Descriptor.Digest != created.Descriptor.Digest {
		t.Fatal("delivery changed bundle identity")
	}
	deliveredImage, err := client.Resolve(ctx, destination.host+"/release/bootstrap/images/adapter:proof")
	if err != nil || deliveredImage.Descriptor.Digest != image.Descriptor.Digest {
		t.Fatalf("delivered service image identity: %v", err)
	}
	verifyGraph(t, ctx, image.Store, deliveredImage.Store, image.Descriptor)
	binding := destination.host + "/release/bootstrap/charts/adapter@" + chart.Descriptor.Digest.String()
	pullDir := t.TempDir()
	helm.run(nil, "pull", "oci://"+binding, "--plain-http", "--destination", pullDir)
	files, err := filepath.Glob(filepath.Join(pullDir, "*.tgz"))
	if err != nil || len(files) != 1 {
		t.Fatalf("pulled chart archive: %v (%v)", files, err)
	}
	original, err := os.ReadFile(paths[7])
	if err != nil {
		t.Fatal(err)
	}
	pulled, err := os.ReadFile(files[0])
	if err != nil || !bytes.Equal(original, pulled) {
		t.Fatalf("delivery changed packaged chart bytes: %v", err)
	}
	helm.run(nil, "plugin", "install", "oci://"+destination.host+"/release/bootstrap/tools/bale:0.0.0", "--verify=false", "--plain-http")
	args := []string{"template", "adapter", "oci://" + binding, "--plain-http", "--values", valuePath}
	unrelocated := helm.run(nil, args...)
	rendered := helm.run(nil, append([]string{"bale"}, args...)...)
	destinationImage := deliveredImage.Repository + "@" + image.Descriptor.Digest.String()
	if !bytes.Contains(unrelocated, []byte(image.Repository+":proof")) || !bytes.Contains(rendered, []byte(destinationImage)) || !strings.Contains(string(rendered), "registry.invalid/charts/app:v1") {
		t.Fatalf("bootstrap did not relocate the service image and retain its release config:\n%s", rendered)
	}
	expected := bytes.ReplaceAll(unrelocated, []byte(image.Repository+":proof"), []byte(destinationImage))
	verifyHelmConsumerOutput(t, expected, rendered, nil, nil)
	assertStopped(t, ctx, source)
}

func publishFluxPackageImage(t *testing.T, ctx context.Context, client *bale.Client, layout, host string) bale.Root {
	t.Helper()
	store, err := oci.NewFromFS(ctx, os.DirFS(layout))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(layout, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(raw, &index); err != nil || len(index.Manifests) != 1 {
		t.Fatalf("expected one packaged image root: %+v (%v)", index, err)
	}
	tag := index.Manifests[0].Annotations[v1.AnnotationRefName]
	if tag == "" {
		t.Fatal("packaged image has no root reference annotation")
	}
	descriptor, err := store.Resolve(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}
	root := bale.Root{Repository: "fixture.example/images/bale-flux", Descriptor: descriptor, Store: store, Aliases: []string{"proof"}, Referrers: bale.ReferrerPolicy{Include: "none"}}
	plan, err := client.Plan(ctx, bale.TransferRequest{Roots: []bale.Root{root}, Destination: host, RepositoryMap: map[string]string{root.Repository: "images/bale-flux"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Execute(ctx, plan); err != nil {
		t.Fatal(err)
	}
	root.Repository = host + "/images/bale-flux"
	return root
}
