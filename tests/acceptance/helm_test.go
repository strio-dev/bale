package acceptance_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/content/oci"
)

// TestHelmPluginRegistryAcceptance runs the shipped guest through the packaged
// CLI and installs the native consumer in stock Helm. SDK calls only publish
// artifact fixtures and verify immutable bytes.
func TestHelmPluginRegistryAcceptance(t *testing.T) {
	names := []string{"BALE_ACCEPTANCE_BINARY", "BALE_ACCEPTANCE_DISTRIBUTION", "BALE_ACCEPTANCE_ZOT", "BALE_ACCEPTANCE_HELM_PLUGIN", "BALE_ACCEPTANCE_HELM", "BALE_ACCEPTANCE_HELM_CONSUMER_ARCHIVE"}
	paths := make([]string, len(names))
	present := 0
	for i, name := range names {
		paths[i] = os.Getenv(name)
		if paths[i] != "" {
			present++
		}
	}
	if present == 0 {
		t.Skip("run the registry acceptance app to supply Bale, registries and the production Helm discovery package")
	}
	if present != len(names) {
		t.Fatal("all acceptance executable paths, Helm discovery package and native Helm consumer archive must be supplied")
	}
	for i, path := range paths {
		info, err := os.Stat(path)
		if err != nil || !filepath.IsAbs(path) || !info.Mode().IsRegular() || ((i < 3 || i == 4) && info.Mode()&0111 == 0) {
			t.Fatalf("%s must name an absolute file (executable for CLI, Helm and registries): %q (%v)", names[i], path, err)
		}
	}
	for _, backend := range []struct{ name, binary string }{{"distribution", paths[1]}, {"zot", paths[2]}} {
		t.Run(backend.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			runHelmRegistryCycle(t, ctx, paths[0], paths[3], paths[4], paths[5], backend.name, backend.binary)
		})
	}
}

func runHelmRegistryCycle(t *testing.T, ctx context.Context, binary, discoveryPackage, helm, consumer, kind, backend string) {
	source := startRegistry(t, ctx, kind, backend, "Helm source")
	destination := startRegistry(t, ctx, kind, backend, "Helm destination")
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	if err := os.Mkdir(docker, 0700); err != nil {
		t.Fatal(err)
	}
	command := cli{t: t, ctx: ctx, binary: binary, env: []string{"PATH=/nonexistent", "DOCKER_CONFIG=" + docker, "XDG_CACHE_HOME=" + filepath.Join(dir, "capture-cache"), "XDG_DATA_HOME=" + filepath.Join(dir, "data")}, hosts: []string{source.host, destination.host}}
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: command.hosts, HTTPClient: &http.Client{Timeout: 10 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	consumerRoot := seedHelmConsumerPackage(t, ctx, client, consumer, source.host)
	// Distinct multi-platform graphs exercise index traversal and shared layers.
	main := seed(t, ctx, client, source.host, "helm-app", "main", true)
	initImage := seed(t, ctx, client, source.host, "helm-init", "init", true)
	hookImage := seed(t, ctx, client, source.host, "helm-hook", "hook", true)
	chart := helmAcceptanceChart(t)
	chartRoot := seedHelmAcceptanceChart(t, ctx, client, source.host, chart)
	pluginRef := source.host + "/plugins/helm:v1"
	published := decode[struct {
		Descriptor v1.Descriptor `json:"descriptor"`
	}](t, command.run("publish production Helm discovery package", "plugins", "push", "-f", discoveryPackage, pluginRef, "--format", "json"))
	producer, err := client.Resolve(ctx, pluginRef)
	if err != nil || producer.Descriptor.Digest != published.Descriptor.Digest {
		t.Fatalf("published plugin identity: %v", err)
	}
	recipe := filepath.Join(dir, "bale.yaml")
	writeHelmAcceptanceRecipe(t, recipe, chartRoot.Repository+":1.0.0", pluginRef, main.Repository+":stable", initImage.Repository+":stable", hookImage.Repository+":stable", consumerRoot.Repository+":0.0.0")
	original := filepath.Join(dir, "capture")
	deliveryPath := filepath.Join(dir, "bale.delivery.yaml")
	captured := decode[bale.CreateResult](t, command.run("capture Helm chart and discovered graphs", "create", "--layout", "-f", recipe, original, "--output-delivery", deliveryPath, "--format", "json"))
	if captured.Published || captured.LayoutPath != original {
		t.Fatal("capture did not remain local")
	}
	verifyHelmCapture(t, ctx, captured, original, chartRoot, producer, []bale.Root{main, initImage, hookImage})
	source.stop(t)
	assertStopped(t, ctx, source)
	offline := command
	offline.hosts = nil
	archive := filepath.Join(dir, "release.tar")
	saved := decode[bale.ArchiveResult](t, offline.run("save Helm bundle offline", "save", "--layout", original, "-o", archive, "--format", "json"))
	if saved.Descriptor.Digest != captured.Descriptor.Digest {
		t.Fatal("save changed snapshot")
	}
	assertArchiveDeduplicated(t, archive)
	if err := os.RemoveAll(original); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored")
	loaded := decode[bale.ArchiveResult](t, offline.run("load Helm bundle offline", "load", "--layout", "-i", archive, restored, "--format", "json"))
	if loaded.Descriptor.Digest != captured.Descriptor.Digest {
		t.Fatal("load changed snapshot")
	}
	raw, err := os.ReadFile(deliveryPath)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := bale.DecodeDeliveryFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	producer.Store = oci.NewStorageFromFS(os.DirFS(restored))
	roots := append([]bale.Root{chartRoot, main, initImage, hookImage, producer}, consumerRoot)
	paths := []string{"charts/app", "images/app", "images/init", "images/hook", "plugins/helm", "tools/bale"}
	for i, root := range roots {
		found := false
		for j := range delivery.Artifacts {
			row := &delivery.Artifacts[j]
			if row.Ref == root.Repository+"@"+root.Descriptor.Digest.String() {
				row.Path = paths[i]
				found = true
			}
		}
		if !found {
			t.Fatalf("delivery omitted %s", root.Repository)
		}
	}
	raw, err = bale.EncodeDeliveryFile(delivery)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, deliveryPath, raw)
	completion := destination.host + "/helm-release:bundle"
	pushed := decode[bale.PushResult](t, command.run("deliver restored Helm bundle", "push", "--layout", restored, completion, "--delivery", deliveryPath, "--format", "json"))
	if pushed.Descriptor.Digest != captured.Descriptor.Digest {
		t.Fatal("delivery changed snapshot")
	}
	delivered := make([]bale.Root, len(roots))
	for i, root := range roots {
		placement, ok := pushed.Relocations.Lookup(bale.ArtifactID{Repository: root.Repository, Digest: root.Descriptor.Digest})
		expected := destination.host + "/helm-release/" + paths[i]
		if !ok || placement.Destination.Repository != expected || placement.Destination.Digest != root.Descriptor.Digest {
			t.Fatalf("incorrect placement for %s: %+v", root.Repository, placement)
		}
		tag := "stable"
		if i == 0 {
			tag = "1.0.0"
		}
		if i == 4 {
			tag = "v1"
		}
		if i >= 5 {
			tag = "0.0.0"
		}
		delivered[i], err = client.Resolve(ctx, expected+":"+tag)
		if err != nil || delivered[i].Descriptor.Digest != root.Descriptor.Digest {
			t.Fatalf("restored tag %s:%s: %v", expected, tag, err)
		}
		verifyGraph(t, ctx, root.Store, delivered[i].Store, root.Descriptor)
	}
	records := decode[[]bale.InspectedDestination](t, command.run("inspect delivered chart dependencies", "destinations", "inspect", delivered[0].Repository+"@"+chartRoot.Descriptor.Digest.String()))
	if len(records) != 1 || len(records[0].Record.Resolutions) != 1 {
		t.Fatalf("chart destination records: %+v", records)
	}
	record := records[0].Record
	if record.Source != chartRoot.Repository+"@"+chartRoot.Descriptor.Digest.String() || record.Bundle != destination.host+"/helm-release@"+captured.Descriptor.Digest.String() {
		t.Fatalf("wrong chart destination provenance: %+v", record)
	}
	resolution := record.Resolutions[0]
	if resolution.Outcome != "selected" || resolution.Declaration == nil || resolution.Declaration.Digest != captured.Metadata.Plugins[0].Declaration.Digest || len(resolution.Dependencies) != 3 {
		t.Fatalf("wrong delivered declaration: %+v", resolution)
	}
	for i, root := range []bale.Root{main, initImage, hookImage} {
		found := false
		for _, dependency := range resolution.Dependencies {
			if dependency.Ref == root.Repository+":stable" && dependency.Source == root.Repository+"@"+root.Descriptor.Digest.String() && dependency.Destination == delivered[i+1].Repository+"@"+root.Descriptor.Digest.String() {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing delivered dependency mapping for %s", root.Repository)
		}
	}
	// Remove all transport copies. A fresh cache must compile the delivered Wasm;
	// chart values explicitly select destination images without rewriting archives.
	for _, path := range []string{restored, archive} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	runHelmConsumerAcceptance(t, ctx, helm, delivered[5], delivered[0], records, []bale.Root{main, initImage, hookImage}, delivered[1:4])
	freshCache := filepath.Join(dir, "reuse-cache")
	if _, err := os.Stat(freshCache); !os.IsNotExist(err) {
		t.Fatal("reuse cache already exists")
	}
	command.env = []string{"PATH=/nonexistent", "DOCKER_CONFIG=" + docker, "XDG_CACHE_HOME=" + freshCache, "XDG_DATA_HOME=" + filepath.Join(dir, "data")}
	command.hosts = []string{destination.host}
	writeHelmAcceptanceRecipe(t, recipe, delivered[0].Repository+":1.0.0", delivered[4].Repository+":v1", delivered[1].Repository+":stable", delivered[2].Repository+":stable", delivered[3].Repository+":stable", delivered[5].Repository+":0.0.0")
	reusePath := filepath.Join(dir, "destination-only")
	reused := decode[bale.CreateResult](t, command.run("reuse delivered Helm guest with fresh cache", "create", "--layout", "-f", recipe, reusePath, "--format", "json"))
	verifyHelmCapture(t, ctx, reused, reusePath, delivered[0], delivered[4], delivered[1:4])
	for _, origin := range reused.Metadata.Origins {
		if !strings.HasPrefix(origin.Repository, destination.host+"/") {
			t.Fatalf("reuse captured non-destination origin: %+v", origin)
		}
	}
	assertStopped(t, ctx, source)
}

func verifyHelmCapture(t *testing.T, ctx context.Context, created bale.CreateResult, layout string, chart, producer bale.Root, images []bale.Root) {
	t.Helper()
	if len(created.Metadata.Plugins) != 1 {
		t.Fatalf("plugin locks: %+v", created.Metadata.Plugins)
	}
	lock := created.Metadata.Plugins[0]
	if lock.Descriptor.Digest != producer.Descriptor.Digest || lock.Subject.Digest != chart.Descriptor.Digest || lock.Ref != producer.Repository+":v1" {
		t.Fatalf("wrong plugin lock: %+v", lock)
	}
	selected := 0
	for _, resolution := range created.Metadata.Dependencies {
		if resolution.Subject.Digest != chart.Descriptor.Digest || resolution.Outcome != "selected" {
			continue
		}
		selected++
		if resolution.Declaration == nil || resolution.Declaration.Digest != lock.Declaration.Digest || len(resolution.Dependencies) != len(images) {
			t.Fatalf("wrong dependency lock: %+v", resolution)
		}
		for _, image := range images {
			found := false
			for _, dependency := range resolution.Dependencies {
				if dependency.Ref == image.Repository+":stable" && dependency.Descriptor.Digest == image.Descriptor.Digest {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing image lock: %s", image.Repository)
			}
		}
	}
	if selected != 1 {
		t.Fatalf("selected declarations: %d", selected)
	}
	store := oci.NewStorageFromFS(os.DirFS(layout))
	for _, root := range append([]bale.Root{chart, producer}, images...) {
		verifyGraph(t, ctx, root.Store, store, root.Descriptor)
	}
	raw, err := content.FetchAll(ctx, store, created.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	for _, image := range images {
		count := 0
		for _, member := range index.Manifests {
			if member.Digest == image.Descriptor.Digest {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("image graph %s occurs %d times", image.Repository, count)
		}
	}
}

func writeHelmAcceptanceRecipe(t *testing.T, path, chart, plugin, main, initImage, hook string, extraRefs ...string) {
	t.Helper()
	// JSON is valid YAML and keeps registry references and plugin input unambiguous.
	recipe := map[string]any{
		"include": []any{
			map[string]any{
				"ref": chart,
				"plugin": map[string]any{
					"ref": plugin,
					"input": map[string]any{
						"schemaVersion": 1,
						"release":       "acceptance",
						"namespace":     "default",
						"context":       "acceptance",
						"values": map[string]any{
							"image":  main,
							"helper": initImage,
							"child":  map[string]string{"image": hook},
						},
					},
					"blobs": map[string]any{
						"chart": map[string]string{"mediaType": "application/vnd.cncf.helm.chart.content.v1.tar+gzip"},
					},
				},
			},
		},
	}
	for _, ref := range extraRefs {
		recipe["include"] = append(recipe["include"].([]any), map[string]any{"ref": ref})
	}
	raw, err := json.Marshal(recipe)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, raw)
}

func helmAcceptanceChart(t *testing.T) []byte {
	t.Helper()
	files := map[string]string{
		"app/Chart.yaml":                       "apiVersion: v2\nname: app\nversion: 1.0.0\ndependencies:\n  - name: child\n    version: 1.0.0\n    repository: file://charts/child\n",
		"app/templates/pod.yaml":               "apiVersion: v1\nkind: Pod\nmetadata:\n  name: {{ .Release.Name }}\nspec:\n  containers:\n    - name: app\n      image: {{ .Values.image | quote }}\n    - name: repeated-app\n      image: {{ .Values.image | quote }}\n  initContainers:\n    - name: init\n      image: {{ .Values.helper | quote }}\n",
		"app/charts/child/Chart.yaml":          "apiVersion: v2\nname: child\nversion: 1.0.0\n",
		"app/charts/child/templates/hook.yaml": "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: test\n  annotations:\n    helm.sh/hook: test\nspec:\n  template:\n    spec:\n      restartPolicy: Never\n      containers:\n        - name: hook\n          image: {{ .Values.image | quote }}\n",
	}
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		raw := []byte(files[name])
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(raw))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func seedHelmAcceptanceChart(t *testing.T, ctx context.Context, client *bale.Client, host string, chart []byte) bale.Root {
	t.Helper()
	store := memory.New()
	put := func(media string, raw []byte) v1.Descriptor {
		d := content.NewDescriptorFromBytes(media, raw)
		if err := store.Push(ctx, d, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		return d
	}
	config := put("application/vnd.cncf.helm.config.v1+json", []byte(`{"apiVersion":"v2","name":"app","version":"1.0.0"}`))
	layer := put("application/vnd.cncf.helm.chart.content.v1.tar+gzip", chart)
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	root := bale.Root{Repository: "fixture.example/charts/app", Descriptor: put(v1.MediaTypeImageManifest, raw), Store: store, Aliases: []string{"1.0.0"}, Referrers: bale.ReferrerPolicy{Include: "none"}}
	plan, err := client.Plan(ctx, bale.TransferRequest{Roots: []bale.Root{root}, Destination: host, RepositoryMap: map[string]string{root.Repository: "charts/app"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Execute(ctx, plan); err != nil {
		t.Fatal(err)
	}
	root.Repository = host + "/charts/app"
	return root
}
