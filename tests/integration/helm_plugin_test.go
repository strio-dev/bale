package integration_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/plugin"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/content/oci"
)

// Ordinary SDK tests do not build or download a plugin. The Nix Helm integration
// check supplies the production guest; a supplied but broken guest must fail.
func helmPlugin(t *testing.T) (string, plugin.Options) {
	t.Helper()
	path := os.Getenv("BALE_HELM_PLUGIN")
	if path == "" {
		t.Skip("BALE_HELM_PLUGIN is unset")
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("BALE_HELM_PLUGIN must identify a built production Wasm guest: %v", err)
	}
	cache := t.TempDir()
	if err := os.Chmod(cache, 0700); err != nil {
		t.Fatal(err)
	}
	return path, plugin.Options{CacheDirectory: cache}
}

func runHelmPlugin(t *testing.T, path string, options plugin.Options, input map[string]any) bale.DependencyDeclaration {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := plugin.Run(t.Context(), plugin.Request{ModulePath: path, Input: raw}, options)
	if err != nil {
		t.Fatal(err)
	}
	return declaration
}

func helmRefs(declaration bale.DependencyDeclaration) []string {
	refs := make([]string, len(declaration.Dependencies))
	for i, dependency := range declaration.Dependencies {
		refs[i] = dependency.Ref
	}
	sort.Strings(refs)
	return refs
}

func TestHelmPluginPodinfo(t *testing.T) {
	path, options := helmPlugin(t)
	chart, err := os.ReadFile("testdata/helm/podinfo-6.9.4.tgz")
	if err != nil {
		t.Fatal(err)
	}
	if digest.FromBytes(chart).String() != "sha256:d2caf247bdb0b3c7813e1b26d235245aca90794f94a133c0101cec4857d2722d" {
		t.Fatal("upstream chart fixture changed")
	}
	hooks := []string{"docker.io/curlimages/curl:7.69.0", "docker.io/giantswarm/tiny-tools:latest", "docker.io/stefanprodan/grpc_health_probe:v0.3.0"}
	for _, tc := range []struct {
		name, image string
		values      map[string]any
	}{
		{name: "defaults", image: "ghcr.io/stefanprodan/podinfo:6.9.4", values: map[string]any{}},
		{name: "digest-pinned", image: "registry.example.com/app@sha256:" + strings.Repeat("a", 64), values: map[string]any{"image": map[string]any{"repository": "registry.example.com/app", "tag": "6.9.4@sha256:" + strings.Repeat("a", 64)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			declaration := runHelmPlugin(t, path, options, map[string]any{"schemaVersion": 1, "chart": chart, "values": tc.values, "release": "bale-proof", "namespace": "default", "capabilities": map[string]any{"kubeVersion": "1.35.0", "apiVersions": []string{}}})
			want := append(append([]string{}, hooks...), tc.image)
			sort.Strings(want)
			if declaration.Context != "helm-render" || !reflect.DeepEqual(helmRefs(declaration), want) {
				t.Fatalf("declaration = %+v; expected refs %v", declaration, want)
			}
		})
	}
}

func TestHelmPluginDefaultCapabilities(t *testing.T) {
	path, options := helmPlugin(t)
	chart := helmChartArchive(t, map[string]string{
		"app/Chart.yaml": "apiVersion: v2\nname: app\nversion: 1.0.0\n",
		"app/templates/pod.yaml": `{{ $priority := "" }}
{{ range .Capabilities.APIVersions }}{{ if and (hasPrefix "resource.k8s.io/v1beta" .) (eq $priority "") }}{{ $priority = trimPrefix "resource.k8s.io/" . }}{{ end }}{{ end }}
apiVersion: v1
kind: Pod
spec:
  containers:
    - name: defaults
      image: registry.example.com/capabilities:{{ trimPrefix "v" .Capabilities.KubeVersion.Version }}-{{ $priority }}
    - name: apps
      image: registry.example.com/apps:{{ if .Capabilities.APIVersions.Has "apps/v1" }}present{{ else }}missing{{ end }}
`,
	})
	declaration := runHelmPlugin(t, path, options, map[string]any{
		"schemaVersion": 1, "chart": chart, "release": "capabilities", "namespace": "default",
	})
	// Pinned client-go v0.37.0 defaults to Kubernetes 1.37 and prioritizes
	// resource.k8s.io/v1beta2 before v1beta1. Both can select chart images.
	want := []string{"registry.example.com/apps:present", "registry.example.com/capabilities:1.37.0-v1beta2"}
	if !reflect.DeepEqual(helmRefs(declaration), want) {
		t.Fatalf("default capabilities selected %v; want %v", helmRefs(declaration), want)
	}
}

func helmChartArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := []byte(files[name])
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func helmChartRoot(t *testing.T, chart []byte, name, version string) (bale.Root, v1.Descriptor) {
	t.Helper()
	store := memory.New()
	put := func(media string, raw []byte) v1.Descriptor {
		d := content.NewDescriptorFromBytes(media, raw)
		if err := store.Push(t.Context(), d, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		return d
	}
	configRaw, err := json.Marshal(map[string]string{"name": name, "version": version, "apiVersion": "v2"})
	if err != nil {
		t.Fatal(err)
	}
	config := put("application/vnd.cncf.helm.config.v1+json", configRaw)
	layer := put("application/vnd.cncf.helm.chart.content.v1.tar+gzip", chart)
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	return bale.Root{Repository: "fixture.example/charts/" + name, Descriptor: put(v1.MediaTypeImageManifest, raw), Store: store, Aliases: []string{version}, Referrers: bale.ReferrerPolicy{Include: "none"}}, layer
}

func TestHelmPluginOfflineBundleDelivery(t *testing.T) {
	path, options := helmPlugin(t)
	sourceRegistry, holdingRegistry := &deliveryRegistry{}, &deliveryRegistry{}
	source, holding := httptest.NewServer(sourceRegistry), httptest.NewServer(holdingRegistry)
	defer source.Close()
	defer holding.Close()
	sourceHost, holdingHost := strings.TrimPrefix(source.URL, "http://"), strings.TrimPrefix(holding.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{sourceHost, holdingHost}})
	if err != nil {
		t.Fatal(err)
	}
	// Two different source repositories deliberately share one exact multiarch
	// graph, while repeated template occurrences share a selector as well.
	images := map[string]bale.Root{}
	for _, name := range []string{"main", "init", "ephemeral", "hook", "child"} {
		root := dependencyArtifact(t, "shared-image", true)
		root.Repository = "fixture.example/images/" + name
		root.Aliases = []string{"stable"}
		plan, err := client.Plan(t.Context(), bale.TransferRequest{Roots: []bale.Root{root}, Destination: sourceHost + "/source"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Execute(t.Context(), plan); err != nil {
			t.Fatal(err)
		}
		resolved, err := client.Resolve(t.Context(), sourceHost+"/source/images/"+name+":stable")
		if err != nil {
			t.Fatal(err)
		}
		images[name] = resolved
	}
	imageRef := func(name string) string { return images[name].Repository + ":stable" }
	chart := helmChartArchive(t, map[string]string{
		"parent/Chart.yaml":  "apiVersion: v2\nname: parent\nversion: 1.0.0\ndependencies:\n  - name: child\n    version: 1.0.0\n    repository: file://charts/child\n    condition: child.enabled\n",
		"parent/values.yaml": "child:\n  enabled: true\nprofile: defaults\n",
		"parent/templates/pod.yaml": `apiVersion: v1
kind: Pod
metadata:
  name: {{ .Release.Name }}
  namespace: {{ .Release.Namespace }}
spec:
  containers:
    - name: main
      image: {{ .Values.main | quote }}
    - name: duplicate
      image: {{ .Values.main | quote }}
  initContainers:
    - name: init
      image: {{ .Values.init | quote }}
  ephemeralContainers:
    - name: debug
      image: {{ .Values.ephemeral | quote }}
`,
		"parent/templates/hook.yaml": `apiVersion: batch/v1
kind: Job
metadata:
  name: test-hook
  annotations:
    helm.sh/hook: test
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: hook
          image: {{ .Values.hook | quote }}
`,
		"parent/charts/child/Chart.yaml": "apiVersion: v2\nname: child\nversion: 1.0.0\n",
		"parent/charts/child/templates/pod.yaml": `apiVersion: v1
kind: Pod
metadata:
  name: nested
spec:
  containers:
    - name: child
      image: {{ .Values.image | quote }}
`,
	})
	// Ordered values files are applied before inline overrides. The earlier
	// disabled child and invalid main image must not affect the final profile.
	declaration := runHelmPlugin(t, path, options, map[string]any{"schemaVersion": 1, "chart": chart, "release": "offline", "namespace": "testing", "context": "production", "valuesFiles": []string{"child:\n  enabled: false\nmain: rejected.invalid/image:old\n", "child:\n  enabled: true\n"}, "values": map[string]any{"main": imageRef("main"), "init": imageRef("init"), "ephemeral": imageRef("ephemeral"), "hook": imageRef("hook"), "child": map[string]any{"image": imageRef("child")}}})
	want := []string{}
	for name := range images {
		want = append(want, imageRef(name))
	}
	sort.Strings(want)
	if declaration.Context != "production" || !reflect.DeepEqual(helmRefs(declaration), want) {
		t.Fatalf("unexpected Helm dependencies: %+v; wanted %v", declaration, want)
	}
	root, chartLayer := helmChartRoot(t, chart, "parent", "1.0.0")
	publish, err := client.Plan(t.Context(), bale.TransferRequest{Roots: []bale.Root{root}, Destination: sourceHost + "/source"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Execute(t.Context(), publish); err != nil {
		t.Fatal(err)
	}
	subject, err := client.Resolve(t.Context(), sourceHost+"/source/charts/parent:1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := bale.PrepareDependencyDeclaration(t.Context(), subject, declaration)
	if err != nil {
		t.Fatal(err)
	}
	if attached, err := client.Attach(t.Context(), prepared); err != nil || !attached.Discoverable {
		t.Fatalf("attachment = %+v, error = %v", attached, err)
	}
	recipe := bale.Recipe{Include: []bale.Selection{{Ref: subject.Repository + ":1.0.0", Dependencies: bale.DependencyPolicy{Select: &bale.DependencySelector{Context: "production"}}}}}
	reference := holdingHost + "/bundles:release"
	created, err := client.Create(t.Context(), recipe, reference, bale.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	selected := 0
	for _, resolution := range created.Metadata.Dependencies {
		if resolution.Outcome == "selected" {
			selected++
			if resolution.Declaration == nil || resolution.Declaration.Digest != prepared.Descriptor.Digest || len(resolution.Dependencies) != len(want) {
				t.Fatalf("lost declaration selection: %+v", resolution)
			}
			if resolution.Policy.Select == nil || resolution.Policy.Select.Context != "production" {
				t.Fatalf("lost context selector: %+v", resolution.Policy)
			}
			capturedRefs := make([]string, 0, len(resolution.Dependencies))
			for _, dependency := range resolution.Dependencies {
				capturedRefs = append(capturedRefs, dependency.Ref)
				if dependency.Descriptor.Digest != images["main"].Descriptor.Digest {
					t.Fatalf("wrong locked image: %+v", dependency)
				}
			}
			sort.Strings(capturedRefs)
			if !reflect.DeepEqual(capturedRefs, want) {
				t.Fatalf("captured selectors = %v; wanted %v", capturedRefs, want)
			}
		}
	}
	if selected != 1 {
		t.Fatalf("selected declarations = %d", selected)
	}
	bundleStore := holdingRegistry.repository("bundles")
	bundleStore.mu.Lock()
	bundleBytes := append([]byte(nil), bundleStore.objects[created.Descriptor.Digest.String()]...)
	bundleStore.mu.Unlock()
	var index v1.Index
	if err := json.Unmarshal(bundleBytes, &index); err != nil {
		t.Fatal(err)
	}
	occurrences := 0
	for _, member := range index.Manifests {
		if member.Digest == images["main"].Descriptor.Digest {
			occurrences++
		}
	}
	if occurrences != 1 {
		t.Fatalf("shared graph appears %d times in bundle", occurrences)
	}
	source.Close()
	lockedClient := destinationTestClient(t, holdingHost, nil)
	layout := filepath.Join(t.TempDir(), "locked")
	locked, err := lockedClient.Pull(t.Context(), reference, layout, bale.PullLocked)
	if err != nil || locked.Descriptor.Digest != created.Descriptor.Digest {
		t.Fatalf("locked = %+v, error = %v", locked, err)
	}
	holding.Close()
	destinationRegistry := &deliveryRegistry{}
	destination := httptest.NewServer(destinationRegistry)
	defer destination.Close()
	destinationHost := strings.TrimPrefix(destination.URL, "http://")
	target := destinationTestClient(t, destinationHost, nil)
	placement := map[string]string{subject.Repository: "charts/parent"}
	for name, image := range images {
		placement[image.Repository] = "images/" + name
	}
	plan, err := target.PlanPush(t.Context(), layout, destinationHost+"/release:complete", bale.PushOptions{RepositoryMap: placement})
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := target.Push(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	relocatedChart, ok := delivered.Relocations.Lookup(bale.ArtifactID{Repository: subject.Repository, Digest: subject.Descriptor.Digest})
	if !ok || relocatedChart.Destination.Digest != subject.Descriptor.Digest {
		t.Fatal("chart manifest changed or missing")
	}
	chartStore := destinationRegistry.repository("release/charts/parent")
	chartStore.mu.Lock()
	deliveredChart := append([]byte(nil), chartStore.objects[chartLayer.Digest.String()]...)
	chartStore.mu.Unlock()
	if !bytes.Equal(chart, deliveredChart) {
		t.Fatal("chart archive bytes changed")
	}
	records, err := target.InspectDestinations(t.Context(), relocatedChart.Destination.Repository+"@"+subject.Descriptor.Digest.String())
	if err != nil || len(records) != 1 {
		t.Fatalf("chart destination records = %+v, error = %v", records, err)
	}
	record := records[0].Record
	if record.Source != destinationRef(subject) || record.Bundle != destinationHost+"/release@"+created.Descriptor.Digest.String() || len(record.Resolutions) != 1 {
		t.Fatalf("wrong chart destination record: %+v", record)
	}
	resolution := record.Resolutions[0]
	if resolution.Outcome != "selected" || resolution.Declaration == nil || resolution.Declaration.Context != "production" || resolution.Declaration.Digest != prepared.Descriptor.Digest || len(resolution.Dependencies) != len(want) {
		t.Fatalf("lost producer context or direct dependencies: %+v", resolution)
	}
	recorded := map[string]string{}
	for _, dependency := range resolution.Dependencies {
		recorded[dependency.Ref] = dependency.Destination
	}
	for name, image := range images {
		expectedRepository := destinationHost + "/release/images/" + name
		if recorded[imageRef(name)] != expectedRepository+"@"+image.Descriptor.Digest.String() {
			t.Fatalf("missing mapping for %s: %+v", name, recorded)
		}
		remote := destinationRegistry.repository("release/images/" + name)
		remote.mu.Lock()
		raw := append([]byte(nil), remote.objects[image.Descriptor.Digest.String()]...)
		remote.mu.Unlock()
		originalIndex, err := content.FetchAll(t.Context(), oci.NewStorageFromFS(os.DirFS(layout)), image.Descriptor)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(originalIndex, raw) {
			t.Fatalf("%s image index changed", name)
		}
		var imageIndex v1.Index
		if err := json.Unmarshal(raw, &imageIndex); err != nil {
			t.Fatal(err)
		}
		if len(imageIndex.Manifests) != 2 {
			t.Fatalf("%s lost platforms", name)
		}
		for _, platform := range imageIndex.Manifests {
			original, err := content.FetchAll(context.Background(), oci.NewStorageFromFS(os.DirFS(layout)), platform)
			if err != nil {
				t.Fatal(err)
			}
			remote.mu.Lock()
			transferred := append([]byte(nil), remote.objects[platform.Digest.String()]...)
			remote.mu.Unlock()
			if !bytes.Equal(original, transferred) {
				t.Fatalf("%s/%s platform manifest changed", name, platform.Platform.Architecture)
			}
			var manifest v1.Manifest
			if err := json.Unmarshal(original, &manifest); err != nil {
				t.Fatal(err)
			}
			for _, blob := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
				originalBlob, err := content.FetchAll(t.Context(), oci.NewStorageFromFS(os.DirFS(layout)), blob)
				if err != nil {
					t.Fatal(err)
				}
				remote.mu.Lock()
				transferredBlob := append([]byte(nil), remote.objects[blob.Digest.String()]...)
				remote.mu.Unlock()
				if !bytes.Equal(originalBlob, transferredBlob) {
					t.Fatalf("%s/%s blob changed: %s", name, platform.Platform.Architecture, blob.Digest)
				}
			}
		}
	}
}

func TestHelmPluginFailures(t *testing.T) {
	path, options := helmPlugin(t)
	for _, tc := range []struct{ name, template, marker string }{
		{"required", `{{ required "required token" .Values.token }}`, "required token"},
		{"template-failure", `{{ fail "explicit template failure" }}`, "explicit template failure"},
		{"invalid-image", "apiVersion: v1\nkind: Pod\nspec:\n  containers: [{image: 'INVALID IMAGE'}]\n", `image "INVALID IMAGE"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chart := helmChartArchive(t, map[string]string{"failure/Chart.yaml": "apiVersion: v2\nname: failure\nversion: 1.0.0\n", "failure/templates/pod.yaml": tc.template})
			input, err := json.Marshal(map[string]any{"schemaVersion": 1, "chart": chart, "release": "failure", "namespace": "default"})
			if err != nil {
				t.Fatal(err)
			}
			declaration, err := plugin.Run(t.Context(), plugin.Request{ModulePath: path, Input: input}, options)
			if err == nil || !strings.Contains(err.Error(), tc.marker) {
				t.Fatalf("expected %q failure; declaration=%+v error=%v", tc.marker, declaration, err)
			}
			if !reflect.DeepEqual(declaration, bale.DependencyDeclaration{}) {
				t.Fatalf("failed guest returned declaration: %+v", declaration)
			}
		})
	}
}

func TestHelmPluginPodinfoOfflineDelivery(t *testing.T) {
	path, options := helmPlugin(t)
	chart, err := os.ReadFile("testdata/helm/podinfo-6.9.4.tgz")
	if err != nil {
		t.Fatal(err)
	}
	sourceRegistry, holdingRegistry := &deliveryRegistry{}, &deliveryRegistry{}
	source, holding := httptest.NewServer(sourceRegistry), httptest.NewServer(holdingRegistry)
	defer source.Close()
	defer holding.Close()
	sourceHost, holdingHost := strings.TrimPrefix(source.URL, "http://"), strings.TrimPrefix(holding.URL, "http://")
	// Preserve the chart's real public hook selectors while serving their fixture
	// graphs from an isolated registry. Every other remote host is denied.
	transport := inspectRoundTripper(func(request *http.Request) (*http.Response, error) {
		next := request.Clone(request.Context())
		location := *request.URL
		next.URL = &location
		switch request.URL.Host {
		case sourceHost, holdingHost:
		case "docker.io", "registry-1.docker.io":
			next.URL.Scheme = "http"
			next.URL.Host = sourceHost
			next.Host = sourceHost
			next.URL.Path = strings.Replace(request.URL.Path, "/v2/", "/v2/source/", 1)
		default:
			return nil, fmt.Errorf("unexpected external registry request: %s", request.URL)
		}
		return http.DefaultTransport.RoundTrip(next)
	})
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{sourceHost, holdingHost, "docker.io"}, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	type imageFixture struct{ repository, tag, path string }
	fixtures := []imageFixture{{"curlimages/curl", "7.69.0", "images/curl"}, {"giantswarm/tiny-tools", "latest", "images/tiny-tools"}, {"stefanprodan/grpc_health_probe", "v0.3.0", "images/grpc-probe"}, {"images/main", "stable", "images/main"}}
	placements := map[string]string{}
	expected := map[string]digest.Digest{}
	for _, fixture := range fixtures {
		root := dependencyArtifact(t, fixture.repository, true)
		root.Repository = "fixture.example/" + fixture.repository
		root.Aliases = []string{fixture.tag}
		plan, err := client.Plan(t.Context(), bale.TransferRequest{Roots: []bale.Root{root}, Destination: sourceHost + "/source"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Execute(t.Context(), plan); err != nil {
			t.Fatal(err)
		}
		repository := "docker.io/" + fixture.repository
		if fixture.repository == "images/main" {
			repository = sourceHost + "/source/" + fixture.repository
		}
		placements[repository] = fixture.path
		expected[repository+":"+fixture.tag] = root.Descriptor.Digest
	}
	declaration := runHelmPlugin(t, path, options, map[string]any{"schemaVersion": 1, "chart": chart, "release": "offline", "namespace": "testing", "context": "podinfo-production", "values": map[string]any{"image": map[string]any{"repository": sourceHost + "/source/images/main", "tag": "stable"}}})
	want := make([]string, 0, len(expected))
	for selector := range expected {
		want = append(want, selector)
	}
	sort.Strings(want)
	if !reflect.DeepEqual(helmRefs(declaration), want) {
		t.Fatalf("real Podinfo refs = %v; wanted %v", helmRefs(declaration), want)
	}
	root, chartLayer := helmChartRoot(t, chart, "podinfo", "6.9.4")
	publication, err := client.Plan(t.Context(), bale.TransferRequest{Roots: []bale.Root{root}, Destination: sourceHost + "/source"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Execute(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	subject, err := client.Resolve(t.Context(), sourceHost+"/source/charts/podinfo:6.9.4")
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := bale.PrepareDependencyDeclaration(t.Context(), subject, declaration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Attach(t.Context(), attachment); err != nil {
		t.Fatal(err)
	}
	reference := holdingHost + "/bundles:podinfo"
	created, err := client.Create(t.Context(), bale.Recipe{Include: []bale.Selection{{Ref: subject.Repository + ":6.9.4"}}}, reference, bale.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	lockedClient := destinationTestClient(t, holdingHost, nil)
	layout := filepath.Join(t.TempDir(), "podinfo")
	locked, err := lockedClient.Pull(t.Context(), reference, layout, bale.PullLocked)
	if err != nil || locked.Descriptor.Digest != created.Descriptor.Digest {
		t.Fatalf("locked pull = %+v, error = %v", locked, err)
	}
	holding.Close()
	registry := &deliveryRegistry{}
	destination := httptest.NewServer(registry)
	defer destination.Close()
	destinationHost := strings.TrimPrefix(destination.URL, "http://")
	target := destinationTestClient(t, destinationHost, nil)
	placements[subject.Repository] = "charts/podinfo"
	plan, err := target.PlanPush(t.Context(), layout, destinationHost+"/release:podinfo", bale.PushOptions{RepositoryMap: placements})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.Push(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	stored := registry.repository("release/charts/podinfo")
	stored.mu.Lock()
	deliveredChart := append([]byte(nil), stored.objects[chartLayer.Digest.String()]...)
	stored.mu.Unlock()
	if !bytes.Equal(chart, deliveredChart) {
		t.Fatal("real Podinfo chart archive changed")
	}
	records, err := target.InspectDestinations(t.Context(), destinationHost+"/release/charts/podinfo@"+subject.Descriptor.Digest.String())
	if err != nil || len(records) != 1 {
		t.Fatalf("Podinfo destination records = %+v, error = %v", records, err)
	}
	record := records[0].Record
	if record.Source != destinationRef(subject) || record.Bundle != destinationHost+"/release@"+created.Descriptor.Digest.String() || len(record.Resolutions) != 1 {
		t.Fatalf("wrong Podinfo destination record: %+v", record)
	}
	resolution := record.Resolutions[0]
	if resolution.Outcome != "selected" || resolution.Declaration == nil || resolution.Declaration.Context != "podinfo-production" || resolution.Declaration.Digest != attachment.Descriptor.Digest || len(resolution.Dependencies) != len(expected) {
		t.Fatalf("lost Podinfo dependency declaration: %+v", resolution)
	}
	for _, dependency := range resolution.Dependencies {
		imageDigest, ok := expected[dependency.Ref]
		if !ok {
			t.Fatalf("unexpected dependency selector: %+v", dependency)
		}
		repository, _, ok := strings.Cut(dependency.Source, "@")
		if !ok {
			t.Fatalf("unpinned source: %+v", dependency)
		}
		expectedDestination := destinationHost + "/release/" + placements[repository] + "@" + imageDigest.String()
		if dependency.Source != repository+"@"+imageDigest.String() || dependency.Destination != expectedDestination {
			t.Fatalf("wrong Podinfo dependency mapping: %+v; wanted %s", dependency, expectedDestination)
		}
		remote := registry.repository("release/" + placements[repository])
		remote.mu.Lock()
		present := len(remote.objects[imageDigest.String()]) > 0
		remote.mu.Unlock()
		if !present {
			t.Fatalf("missing Podinfo image graph: %s", dependency.Destination)
		}
	}
}
