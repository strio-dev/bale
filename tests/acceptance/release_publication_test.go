package acceptance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"go.yaml.in/yaml/v3"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
)

// TestReleasePublicationRegistryAcceptance exercises the shipped release
// commands against real registries. The other-platform inputs are explicitly
// labeled descriptor fixtures; native ARM execution remains a separate CI job.
func TestReleasePublicationRegistryAcceptance(t *testing.T) {
	if os.Getenv("BALE_ACCEPTANCE_INTEGRATION_STAGE") == "" && os.Getenv("BALE_ACCEPTANCE_INTEGRATION_PUBLISH") == "" {
		t.Skip("run registry acceptance with packaged integration release commands")
	}
	names := []string{"BALE_ACCEPTANCE_INTEGRATION_STAGE", "BALE_ACCEPTANCE_INTEGRATION_PUBLISH", "BALE_ACCEPTANCE_REGCTL", "BALE_ACCEPTANCE_PUBLISH", "BALE_ACCEPTANCE_HELM", "BALE_ACCEPTANCE_FLUX_CHART", "BALE_ACCEPTANCE_HELM_CONSUMER_ARCHIVE", "BALE_ACCEPTANCE_DISTRIBUTION", "BALE_ACCEPTANCE_ZOT"}
	paths := make([]string, len(names))
	for i, name := range names {
		paths[i] = os.Getenv(name)
		info, err := os.Stat(paths[i])
		if err != nil || !filepath.IsAbs(paths[i]) || !info.Mode().IsRegular() || (i != 5 && i != 6 && info.Mode()&0111 == 0) {
			t.Fatalf("%s must name its packaged file: %q (%v)", name, paths[i], err)
		}
	}
	for _, backend := range []struct{ name, binary string }{{"distribution", paths[7]}, {"zot", paths[8]}} {
		t.Run(backend.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			runReleasePublication(t, ctx, paths, backend.name, backend.binary)
		})
	}
}

func runReleasePublication(t *testing.T, ctx context.Context, paths []string, kind, backend string) {
	t.Helper()
	registry := startRegistry(t, ctx, kind, backend, "release publication")
	base := registry.host + "/bale"
	dir := t.TempDir()
	env := []string{"PATH=/nonexistent", "HOME=" + dir, "DOCKER_CONFIG=" + filepath.Join(dir, "docker"), "REGCTL_CONFIG=" + filepath.Join(dir, "regctl.json")}
	run := func(binary string, args ...string) []byte {
		t.Helper()
		out, stderr, err := releasePublicationCommand(ctx, env, binary, args...)
		if err != nil {
			t.Fatalf("%s %s: %v\nstdout:\n%s\nstderr:\n%s", filepath.Base(binary), strings.Join(args, " "), err, tail(out, 16000), tail(stderr, 16000))
		}
		return out
	}
	helm := newHelmConsumerCLI(t, ctx, paths[4])
	var chart struct {
		Version    string
		AppVersion string `yaml:"appVersion"`
	}
	if err := yaml.Unmarshal(helm.run(nil, "show", "chart", paths[5]), &chart); err != nil || chart.Version == "" || chart.AppVersion == "" {
		t.Fatalf("release chart metadata: %+v (%v)", chart, err)
	}
	imageTag := strings.ReplaceAll(chart.AppVersion, "+", "_")
	consumerVersion := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(paths[6]), "bale-"), ".tgz")
	consumerTag := strings.ReplaceAll(consumerVersion, "+", "_")
	staged := filepath.Join(dir, "digests")
	run(paths[0], "--plain-http", base, "acceptance", staged)
	readDigest := func(name string) digest.Digest {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(staged, name+".digest"))
		if err != nil {
			t.Fatal(err)
		}
		d, err := digest.Parse(strings.TrimSpace(string(raw)))
		if err != nil || d.Algorithm() != digest.SHA256 {
			t.Fatalf("invalid staged %s digest: %q (%v)", name, raw, err)
		}
		return d
	}
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{registry.host}})
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(ref string) bale.Root {
		t.Helper()
		root, err := client.Resolve(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		return root
	}
	flux := resolve(base + "-flux:build-acceptance-" + runtime.GOARCH)
	consumer := resolve(base + "/plugins/bale:build-acceptance-" + runtime.GOARCH)
	if flux.Descriptor.Digest != readDigest("flux-"+runtime.GOARCH) || consumer.Descriptor.Digest != readDigest("helm-consumer-"+runtime.GOARCH) {
		t.Fatal("staging handoff differs from the uploaded native artifacts")
	}
	fluxManifest := releasePublicationManifest(t, ctx, flux)
	configRaw, err := content.FetchAll(ctx, flux.Store, fluxManifest.Config)
	if err != nil {
		t.Fatal(err)
	}
	var config v1.Image
	if err := json.Unmarshal(configRaw, &config); err != nil || config.Config.Labels[v1.AnnotationSource] != "https://github.com/strio-dev/bale" {
		t.Fatalf("staged Flux image source association: %v", err)
	}
	if releasePublicationManifest(t, ctx, consumer).Annotations[v1.AnnotationSource] != "https://github.com/strio-dev/bale" {
		t.Fatal("staged native Helm plugin lost source association")
	}
	if runtime.GOARCH != "amd64" {
		// Discovery is portable; production publishes it only in the AMD64 job.
		ref := base + "/plugins/helm-discovery:build-acceptance"
		run(paths[3], "helm-discovery", ref, "--to-plain-http")
		writeFile(t, filepath.Join(staged, "helm-discovery.digest"), []byte(resolve(ref).Descriptor.Digest.String()+"\n"))
	}
	discovery := resolve(base + "/plugins/helm-discovery:build-acceptance")
	if discovery.Descriptor.Digest != readDigest("helm-discovery") || releasePublicationManifest(t, ctx, discovery).Annotations[v1.AnnotationSource] != "https://github.com/strio-dev/bale" {
		t.Fatal("staged discovery identity or source association differs")
	}
	other := "arm64"
	if runtime.GOARCH == other {
		other = "amd64"
	}
	fixtureFlux := seedReleasePlatformFixture(t, ctx, base+"-flux", other)
	fixtureConsumer := seedReleaseConsumerFixture(t, ctx, consumer, other)
	writeFile(t, filepath.Join(staged, "flux-"+other+".digest"), []byte(fixtureFlux.Digest.String()+"\n"))
	writeFile(t, filepath.Join(staged, "helm-consumer-"+other+".digest"), []byte(fixtureConsumer.Digest.String()+"\n"))

	finalRefs := map[string]digest.Digest{
		base + "/plugins/helm-discovery:" + imageTag:                       discovery.Descriptor.Digest,
		base + "/plugins/bale:" + consumerTag + "-linux-" + runtime.GOARCH: consumer.Descriptor.Digest,
		base + "/plugins/bale:" + consumerTag + "-linux-" + other:          fixtureConsumer.Digest,
	}
	// Every handoff must resolve before the first version tag is published.
	writeFile(t, filepath.Join(staged, "helm-discovery.digest"), []byte("sha256:"+strings.Repeat("f", 64)+"\n"))
	if _, _, err := releasePublicationCommand(ctx, env, paths[1], "--plain-http", base, staged); err == nil {
		t.Fatal("publication accepted an unavailable final staged digest")
	}
	for _, ref := range append([]string{base + "-flux:" + imageTag, base + "/charts/bale-flux:" + strings.ReplaceAll(chart.Version, "+", "_")}, releasePublicationReferences(finalRefs)...) {
		if _, err := client.Resolve(ctx, ref); err == nil {
			t.Fatalf("preflight failure published %s", ref)
		}
	}
	writeFile(t, filepath.Join(staged, "helm-discovery.digest"), []byte(discovery.Descriptor.Digest.String()+"\n"))
	indexDigest := strings.TrimSpace(string(run(paths[1], "--plain-http", base, staged)))
	indexRaw := run(paths[2], "--host", "reg="+registry.host+",tls=disabled", "manifest", "get", base+"-flux:"+imageTag, "--format", "raw-body")
	var index v1.Index
	if err := json.Unmarshal(indexRaw, &index); err != nil || len(index.Manifests) != 2 {
		t.Fatalf("published Flux platform index: %v\n%s", err, indexRaw)
	}
	if digest.FromBytes(indexRaw).String() != indexDigest {
		t.Fatal("publisher did not report the exact Flux index digest")
	}
	platforms := map[string]digest.Digest{runtime.GOARCH: flux.Descriptor.Digest, other: fixtureFlux.Digest}
	for _, child := range index.Manifests {
		if child.Platform == nil || child.Platform.OS != "linux" || platforms[child.Platform.Architecture] != child.Digest {
			t.Fatalf("incorrect platform descriptor: %+v", child)
		}
		delete(platforms, child.Platform.Architecture)
	}
	if len(platforms) != 0 {
		t.Fatal("published Flux index omitted a platform")
	}
	imageIndex := resolve(base + "-flux:" + imageTag)
	verifyGraph(t, ctx, flux.Store, imageIndex.Store, flux.Descriptor)
	for ref, expected := range finalRefs {
		promoted := resolve(ref)
		if got := promoted.Descriptor.Digest; got != expected {
			t.Fatalf("promoted %s differs from its staged digest: %s", ref, got)
		}
		switch expected {
		case consumer.Descriptor.Digest:
			verifyGraph(t, ctx, consumer.Store, promoted.Store, consumer.Descriptor)
		case discovery.Descriptor.Digest:
			verifyGraph(t, ctx, discovery.Store, promoted.Store, discovery.Descriptor)
		}
	}
	helm.run(nil, "plugin", "install", "oci://"+base+"/plugins/bale:"+consumerTag+"-linux-"+runtime.GOARCH, "--verify=false", "--plain-http")
	if listing := helm.run(nil, "plugin", "list"); !bytes.Contains(listing, []byte(consumerVersion)) {
		t.Fatalf("stock Helm did not install the released native package: %s", listing)
	}
	chartRoot := resolve(base + "/charts/bale-flux:" + strings.ReplaceAll(chart.Version, "+", "_"))
	if releasePublicationManifest(t, ctx, chartRoot).Annotations[v1.AnnotationSource] != "https://github.com/strio-dev/bale" {
		t.Fatal("published chart lost source association")
	}
	rendered := helm.run(nil, "template", "released", "oci://"+base+"/charts/bale-flux", "--version", chart.Version, "--plain-http", "--set", "existingConfigMap=release-test")
	if !bytes.Contains(rendered, []byte("ghcr.io/strio-dev/bale-flux:"+imageTag)) {
		t.Fatalf("released chart default image does not match its version: %s", rendered)
	}
	if retried := strings.TrimSpace(string(run(paths[1], "--plain-http", base, staged))); retried != indexDigest {
		t.Fatalf("retry changed the Flux release index: %s != %s", retried, indexDigest)
	}
	for ref, expected := range finalRefs {
		if got := resolve(ref).Descriptor.Digest; got != expected {
			t.Fatalf("retry changed promoted artifact %s: %s != %s", ref, got, expected)
		}
	}
	if got := resolve(base + "/charts/bale-flux:" + strings.ReplaceAll(chart.Version, "+", "_")).Descriptor.Digest; got != chartRoot.Descriptor.Digest {
		t.Fatalf("retry changed the published chart: %s != %s", got, chartRoot.Descriptor.Digest)
	}
	t.Logf("release publication passed: real %s native packages, descriptor-only %s composition fixture, exact digest promotion, stock Helm install and idempotent retry", runtime.GOARCH, other)
}

func releasePublicationCommand(ctx context.Context, env []string, binary string, args ...string) ([]byte, []byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, binary, args...)
	command.Env = env
	var out, stderr bytes.Buffer
	command.Stdout, command.Stderr = &out, &stderr
	err := command.Run()
	return out.Bytes(), stderr.Bytes(), err
}

func releasePublicationManifest(t *testing.T, ctx context.Context, root bale.Root) v1.Manifest {
	t.Helper()
	raw, err := content.FetchAll(ctx, root.Store, root.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func releasePublicationReferences(refs map[string]digest.Digest) []string {
	result := make([]string, 0, len(refs))
	for ref := range refs {
		result = append(result, ref)
	}
	return result
}

func seedReleasePlatformFixture(t *testing.T, ctx context.Context, repository, arch string) v1.Descriptor {
	t.Helper()
	remote, err := remote.NewRepository(repository)
	if err != nil {
		t.Fatal(err)
	}
	remote.PlainHTTP = true
	put := func(media string, raw []byte) v1.Descriptor {
		t.Helper()
		d := content.NewDescriptorFromBytes(media, raw)
		if err := remote.Push(ctx, d, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		return d
	}
	layer := put(v1.MediaTypeImageLayer, make([]byte, 1024))
	config := v1.Image{Platform: v1.Platform{OS: "linux", Architecture: arch}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer.Digest}}, Config: v1.ImageConfig{Labels: map[string]string{"dev.bale.acceptance.fixture": "descriptor-only; no platform execution"}}}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	manifest := v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: put(v1.MediaTypeImageConfig, raw), Layers: []v1.Descriptor{layer}, Annotations: map[string]string{"dev.bale.acceptance.fixture": "descriptor-only; no platform execution"}}
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return put(v1.MediaTypeImageManifest, raw)
}

func seedReleaseConsumerFixture(t *testing.T, ctx context.Context, root bale.Root, arch string) v1.Descriptor {
	t.Helper()
	manifest := releasePublicationManifest(t, ctx, root)
	if manifest.Annotations == nil {
		manifest.Annotations = map[string]string{}
	}
	manifest.Annotations["dev.bale.acceptance.fixture"] = "descriptor-only " + arch + "; contains host package, never installed"
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, raw)
	remote, err := remote.NewRepository(root.Repository)
	if err != nil {
		t.Fatal(err)
	}
	remote.PlainHTTP = true
	if err := remote.Push(ctx, descriptor, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	return descriptor
}
