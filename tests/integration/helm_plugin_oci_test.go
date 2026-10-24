package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/plugin"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
)

// This exercises the production guest through configured OCI discovery, rather
// than generating and publishing an attachment as a separate authoring step.
func TestHelmPluginOCIConfiguredCapture(t *testing.T) {
	_, options := helmPlugin(t)
	packagePath := os.Getenv("BALE_HELM_PLUGIN_ARCHIVE")
	if info, err := os.Stat(packagePath); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("BALE_HELM_PLUGIN_ARCHIVE must identify a built production discovery package: %v", err)
	}
	sourceRegistry, holdingRegistry := &deliveryRegistry{}, &deliveryRegistry{}
	source, holding := httptest.NewServer(sourceRegistry), httptest.NewServer(holdingRegistry)
	defer source.Close()
	defer holding.Close()
	sourceHost, holdingHost := strings.TrimPrefix(source.URL, "http://"), strings.TrimPrefix(holding.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{sourceHost, holdingHost}, PluginRunner: plugin.Runner(options)})
	if err != nil {
		t.Fatal(err)
	}
	publish := func(root bale.Root, repository, tag string) bale.Root {
		root.Aliases = []string{tag}
		plan, err := client.Plan(t.Context(), bale.TransferRequest{Roots: []bale.Root{root}, Destination: sourceHost, RepositoryMap: map[string]string{root.Repository: repository}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Execute(t.Context(), plan); err != nil {
			t.Fatal(err)
		}
		resolved, err := client.Resolve(t.Context(), sourceHost+"/"+repository+":"+tag)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	image := publish(dependencyArtifact(t, "application image", true), "images/app", "stable")
	packedLayout := t.TempDir()
	store, err := oci.NewStorage(packedLayout)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := plugin.Pack(t.Context(), store, packagePath)
	if err != nil {
		t.Fatal(err)
	}
	producer := publish(bale.Root{Repository: "fixture.example/plugins/helm", Descriptor: descriptor, Store: store, Referrers: bale.ReferrerPolicy{Include: "none"}}, "plugins/helm", "v1")
	chart := helmChartArchive(t, map[string]string{
		"app/Chart.yaml":         "apiVersion: v2\nname: app\nversion: 1.0.0\n",
		"app/templates/pod.yaml": "apiVersion: v1\nkind: Pod\nmetadata:\n  name: {{ .Release.Name }}\nspec:\n  containers:\n    - name: app\n      image: {{ .Values.image | quote }}\n",
	})
	chartRoot, chartLayer := helmChartRoot(t, chart, "app", "1.0.0")
	subject := publish(chartRoot, "charts/app", "1.0.0")
	before, err := client.ListReferrers(t.Context(), subject.Repository+"@"+subject.Descriptor.Digest.String(), bale.ListReferrersOptions{})
	if err != nil || len(before) != 0 {
		t.Fatalf("unexpected source inventory: %+v %v", before, err)
	}
	input, err := json.Marshal(map[string]any{"schemaVersion": 1, "release": "configured", "namespace": "testing", "context": "configured-production", "values": map[string]string{"image": image.Repository + ":stable"}})
	if err != nil {
		t.Fatal(err)
	}
	recipe := bale.Recipe{Include: []bale.Selection{{Ref: subject.Repository + ":1.0.0", Plugin: &bale.Plugin{Ref: producer.Repository + ":v1", Input: input, Blobs: map[string]bale.PluginBlob{"chart": {MediaType: chartLayer.MediaType}}}}}}
	reference := holdingHost + "/bundles:configured"
	created, err := client.Create(t.Context(), recipe, reference, bale.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Metadata.Plugins) != 1 {
		t.Fatalf("missing plugin lock: %+v", created.Metadata.Plugins)
	}
	lockedPlugin := created.Metadata.Plugins[0]
	if lockedPlugin.Descriptor.Digest != producer.Descriptor.Digest || lockedPlugin.Subject.Digest != subject.Descriptor.Digest || lockedPlugin.Ref != producer.Repository+":v1" {
		t.Fatalf("wrong plugin lock: %+v", lockedPlugin)
	}
	selected := false
	for _, resolution := range created.Metadata.Dependencies {
		if resolution.Subject.Digest == subject.Descriptor.Digest && resolution.Outcome == "selected" {
			selected = true
			if resolution.Declaration == nil || resolution.Declaration.Digest != lockedPlugin.Declaration.Digest || len(resolution.Dependencies) != 1 || resolution.Dependencies[0].Descriptor.Digest != image.Descriptor.Digest {
				t.Fatalf("wrong automatic dependency discovery: %+v", resolution)
			}
		}
	}
	if !selected {
		t.Fatal("configured producer declaration was not selected")
	}
	after, err := client.ListReferrers(t.Context(), subject.Repository+"@"+subject.Descriptor.Digest.String(), bale.ListReferrersOptions{})
	if err != nil || len(after) != 0 {
		t.Fatalf("create modified source inventory: %+v %v", after, err)
	}
	source.Close()
	lockedClient := destinationTestClient(t, holdingHost, nil)
	denied := filepath.Join(t.TempDir(), "denied")
	if _, err := lockedClient.Pull(t.Context(), reference, denied, bale.PullLatest); !errors.Is(err, bale.ErrInvalid) {
		t.Fatalf("latest executed without permission: %v", err)
	}
	if _, err := os.Lstat(denied); !os.IsNotExist(err) {
		t.Fatalf("denied refresh created layout: %v", err)
	}
	layout := filepath.Join(t.TempDir(), "locked")
	pulled, err := lockedClient.Pull(t.Context(), reference, layout, bale.PullLocked)
	if err != nil || pulled.Descriptor.Digest != created.Descriptor.Digest {
		t.Fatalf("offline locked pull: %+v %v", pulled, err)
	}
	holding.Close()
	destinationRegistry := &deliveryRegistry{}
	destination := httptest.NewServer(destinationRegistry)
	defer destination.Close()
	destinationHost := strings.TrimPrefix(destination.URL, "http://")
	target := destinationTestClient(t, destinationHost, nil)
	placements := map[string]string{subject.Repository: "charts/app", image.Repository: "images/app", producer.Repository: "plugins/helm"}
	plan, err := target.PlanPush(t.Context(), layout, destinationHost+"/release:complete", bale.PushOptions{RepositoryMap: placements})
	if err != nil {
		t.Fatal(err)
	}
	foundTag := false
	for _, tag := range plan.Tags {
		if tag.Repository == destinationHost+"/release/plugins/helm" && tag.Reference == "v1" && tag.Descriptor.Digest == producer.Descriptor.Digest {
			foundTag = true
		}
	}
	if !foundTag {
		t.Fatal("delivery did not restore producer tag")
	}
	delivered, err := target.Push(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if relocation, ok := delivered.Relocations.Lookup(bale.ArtifactID{Repository: producer.Repository, Digest: producer.Descriptor.Digest}); !ok || relocation.Destination.Repository != destinationHost+"/release/plugins/helm" {
		t.Fatal("portable producer was not delivered")
	}
	chartStore := destinationRegistry.repository("release/charts/app")
	chartStore.mu.Lock()
	actualChart := append([]byte(nil), chartStore.objects[chartLayer.Digest.String()]...)
	chartStore.mu.Unlock()
	if !bytes.Equal(chart, actualChart) {
		t.Fatal("configured capture modified chart bytes")
	}
	local := oci.NewStorageFromFS(os.DirFS(layout))
	manifestBytes, err := content.FetchAll(t.Context(), local, lockedPlugin.Descriptor)
	if err != nil || len(manifestBytes) == 0 {
		t.Fatalf("plugin graph missing offline: %v", err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || len(manifest.Layers) != 1 {
		t.Fatalf("plugin manifest missing archive layer: %v", err)
	}
	declarations, err := target.InspectDependencies(t.Context(), destinationHost+"/release/charts/app@"+subject.Descriptor.Digest.String())
	if err != nil || len(declarations) != 1 || declarations[0].Descriptor.Digest != lockedPlugin.Declaration.Digest {
		t.Fatalf("generated declaration was not delivered beside chart: %+v %v", declarations, err)
	}

	t.Run("reuse delivered plugin", func(t *testing.T) {
		// Remove the original OCI layouts and compiled cache. Only the delivered
		// registry content may supply this capture; both prior registries are closed.
		for _, path := range []string{packedLayout, layout, options.CacheDirectory} {
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
		}
		cache := t.TempDir()
		if err := os.Chmod(cache, 0700); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(cache)
		if err != nil || len(entries) != 0 {
			t.Fatalf("reuse cache must start empty: %v", err)
		}
		freshOptions := options
		freshOptions.CacheDirectory = cache
		run := plugin.Runner(freshOptions)
		var runs, packageDownloads atomic.Int64
		packageBlobPath := "/v2/release/plugins/helm/blobs/" + manifest.Layers[0].Digest.String()
		reuser, err := bale.NewClient(bale.ClientOptions{
			PlainHTTPRegistries: []string{destinationHost},
			HTTPClient: &http.Client{Transport: inspectRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != destinationHost {
					t.Errorf("reuse contacted a historical registry: %s", r.URL)
					return nil, fmt.Errorf("unexpected registry: %s", r.URL.Host)
				}
				if strings.HasPrefix(r.URL.Path, "/v2/release/") && r.Method != http.MethodGet && r.Method != http.MethodHead {
					t.Errorf("reuse wrote to delivered source: %s %s", r.Method, r.URL)
					return nil, errors.New("unexpected delivered source write")
				}
				if r.Method == http.MethodGet && r.URL.Path == packageBlobPath {
					packageDownloads.Add(1)
				}
				return http.DefaultTransport.RoundTrip(r)
			})},
			PluginRunner: func(ctx context.Context, request bale.PluginRequest) (bale.DependencyDeclaration, error) {
				runs.Add(1)
				return run(ctx, request)
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		chartRepository := destinationHost + "/release/charts/app"
		pluginRepository := destinationHost + "/release/plugins/helm"
		imageRepository := destinationHost + "/release/images/app"
		input, err := json.Marshal(map[string]any{
			"schemaVersion": 1, "release": "reused", "namespace": "testing", "context": "reused-production",
			"values": map[string]string{"image": imageRepository + ":stable"},
		})
		if err != nil {
			t.Fatal(err)
		}
		recipe := bale.Recipe{Include: []bale.Selection{{Ref: chartRepository + ":1.0.0", Plugin: &bale.Plugin{
			Ref: pluginRepository + ":v1", Input: input, Blobs: map[string]bale.PluginBlob{"chart": {MediaType: chartLayer.MediaType}},
		}}}}
		bundleRef := destinationHost + "/rebundled:reused"
		recaptured, err := reuser.Create(t.Context(), recipe, bundleRef, bale.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !recaptured.Published || runs.Load() != 1 || packageDownloads.Load() == 0 {
			t.Fatalf("delivered plugin was not executed: published=%t runs=%d package downloads=%d", recaptured.Published, runs.Load(), packageDownloads.Load())
		}
		if len(recaptured.Metadata.Plugins) != 1 {
			t.Fatalf("missing reused plugin pin: %+v", recaptured.Metadata.Plugins)
		}
		pinned := recaptured.Metadata.Plugins[0]
		if pinned.Repository != chartRepository || pinned.Subject.Digest != subject.Descriptor.Digest || pinned.Ref != pluginRepository+":v1" || pinned.Descriptor.Digest != producer.Descriptor.Digest || pinned.Declaration.Digest == lockedPlugin.Declaration.Digest {
			t.Fatalf("incorrect reused plugin provenance: %+v", pinned)
		}
		selected := false
		for _, resolution := range recaptured.Metadata.Dependencies {
			if resolution.Repository == chartRepository && resolution.Subject.Digest == subject.Descriptor.Digest && resolution.Outcome == "selected" {
				selected = true
				if resolution.Declaration == nil || resolution.Declaration.Digest != pinned.Declaration.Digest || len(resolution.Dependencies) != 1 || resolution.Dependencies[0].Ref != imageRepository+":stable" || resolution.Dependencies[0].Descriptor.Digest != image.Descriptor.Digest {
					t.Fatalf("reuse selected historical dependencies: %+v", resolution)
				}
			}
		}
		if !selected {
			t.Fatal("reuse did not select its newly generated declaration")
		}
		for _, origin := range recaptured.Metadata.Origins {
			if !strings.HasPrefix(origin.Repository, destinationHost+"/") {
				t.Fatalf("reuse retained a historical source coordinate: %+v", origin)
			}
		}
		unchanged, err := reuser.InspectDependencies(t.Context(), chartRepository+":1.0.0")
		if err != nil || len(unchanged) != 1 || unchanged[0].Descriptor.Digest != lockedPlugin.Declaration.Digest || len(unchanged[0].Declaration.Dependencies) != 1 || unchanged[0].Declaration.Dependencies[0].Ref != image.Repository+":stable" {
			t.Fatalf("reuse changed historical source declaration: %+v %v", unchanged, err)
		}
		newLayout := filepath.Join(t.TempDir(), "recaptured")
		if _, err := reuser.Pull(t.Context(), bundleRef, newLayout, bale.PullLocked); err != nil {
			t.Fatal(err)
		}
		destination.Close()
		finalRegistry := &deliveryRegistry{}
		final := httptest.NewServer(finalRegistry)
		defer final.Close()
		finalHost := strings.TrimPrefix(final.URL, "http://")
		finalClient := destinationTestClient(t, finalHost, nil)
		plan, err := finalClient.PlanPush(t.Context(), newLayout, finalHost+"/second:complete", bale.PushOptions{RepositoryMap: map[string]string{
			chartRepository: "charts/app", pluginRepository: "plugins/helm", imageRepository: "images/app",
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := finalClient.Push(t.Context(), plan); err != nil {
			t.Fatal(err)
		}
		finalPlugin, err := finalClient.Resolve(t.Context(), finalHost+"/second/plugins/helm:v1")
		if err != nil || finalPlugin.Descriptor.Digest != pinned.Descriptor.Digest {
			t.Fatalf("reused plugin did not survive another offline delivery: %+v %v", finalPlugin, err)
		}
		finalDeclarations, err := finalClient.InspectDependencies(t.Context(), finalHost+"/second/charts/app:1.0.0")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, declaration := range finalDeclarations {
			if declaration.Descriptor.Digest == pinned.Declaration.Digest {
				found = true
				if declaration.Declaration.Context != "reused-production" || len(declaration.Declaration.Dependencies) != 1 || declaration.Declaration.Dependencies[0].Ref != imageRepository+":stable" {
					t.Fatalf("recaptured declaration changed during delivery: %+v", declaration)
				}
			}
		}
		if !found || runs.Load() != 1 {
			t.Fatalf("recapture did not survive locked transfer: declaration=%t plugin runs=%d", found, runs.Load())
		}
	})
}
