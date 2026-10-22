package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

// This fixture deliberately has opaque, application-specific config/payloads.
// Only OCI envelopes and the generic declaration are interpreted by Bale.
func dependencyArtifact(t *testing.T, value string, multi bool) bale.Root {
	t.Helper()
	store := memory.New()
	put := func(mediaType string, raw []byte) v1.Descriptor {
		d := content.NewDescriptorFromBytes(mediaType, raw)
		if err := store.Push(context.Background(), d, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		return d
	}
	config := put("application/example.component.config", []byte("opaque component config"))
	layer := put("application/example.component.payload", []byte(value))
	makeManifest := func(arch string) v1.Descriptor {
		raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{layer}, Annotations: map[string]string{"architecture": arch}})
		if err != nil {
			t.Fatal(err)
		}
		d := put(v1.MediaTypeImageManifest, raw)
		d.Platform = &v1.Platform{OS: "linux", Architecture: arch}
		return d
	}
	root := makeManifest("amd64")
	if multi {
		raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{root, makeManifest("arm64")}})
		if err != nil {
			t.Fatal(err)
		}
		root = put(v1.MediaTypeImageIndex, raw)
	}
	return bale.Root{Repository: "fixture.example/component", Descriptor: root, Store: store, Referrers: bale.ReferrerPolicy{Include: "none"}}
}

func publishDependencyArtifact(t *testing.T, client *bale.Client, host string, root bale.Root, aliases ...string) bale.Root {
	t.Helper()
	root.Aliases = aliases
	plan, err := client.Plan(context.Background(), bale.TransferRequest{Roots: []bale.Root{root}, Destination: host + "/source"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Execute(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	resolved, err := client.Resolve(context.Background(), host+"/source/component@"+root.Descriptor.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestPublicDependencyCaptureLatestAndOfflineDelivery(t *testing.T) {
	ctx := context.Background()
	subjectRegistry, imageRegistry, storageRegistry := newSDKRegistry(), newSDKRegistry(), newSDKRegistry()
	subjectServer, imageServer, storageServer := httptest.NewServer(subjectRegistry), httptest.NewServer(imageRegistry), httptest.NewServer(storageRegistry)
	defer subjectServer.Close()
	defer imageServer.Close()
	defer storageServer.Close()
	subjectHost := strings.TrimPrefix(subjectServer.URL, "http://")
	imageHost := strings.TrimPrefix(imageServer.URL, "http://")
	storageHost := strings.TrimPrefix(storageServer.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{subjectHost, imageHost, storageHost}})
	if err != nil {
		t.Fatal(err)
	}
	subject := publishDependencyArtifact(t, client, subjectHost, dependencyArtifact(t, "component", false), "application")
	image := publishDependencyArtifact(t, client, imageHost, dependencyArtifact(t, "runtime-v1", true), "runtime", "current")
	declaration := bale.DependencyDeclaration{Dependencies: []bale.DependencyReference{{Ref: image.Repository + ":runtime"}, {Ref: image.Repository + ":runtime"}, {Ref: image.Repository + ":current"}}}
	prepared, err := bale.PrepareDependencyDeclaration(ctx, subject, declaration)
	if err != nil {
		t.Fatal(err)
	}
	attached, err := client.Attach(ctx, prepared)
	if err != nil || !attached.Published || !attached.Discoverable {
		t.Fatalf("attach=%+v err=%v", attached, err)
	}
	// Declaring dependencies never uploads them to the subject repository.
	subjectRegistry.mu.Lock()
	_, leaked := subjectRegistry.objects[image.Descriptor.Digest.String()]
	subjectRegistry.mu.Unlock()
	if leaked {
		t.Fatal("declaration publication included dependency content")
	}
	recipe := bale.Recipe{Include: []bale.Selection{{Ref: subject.Repository + ":application", Referrers: bale.ReferrerPolicy{Include: "none"}}}}
	bundleReference := storageHost + "/bundles:original"
	created, err := client.Create(ctx, recipe, bundleReference, bale.CreateOptions{})
	if err != nil || !created.Published {
		t.Fatalf("create=%+v err=%v", created, err)
	}
	selected := 0
	for _, resolution := range created.Metadata.Dependencies {
		if resolution.Outcome != "selected" {
			continue
		}
		selected++
		if resolution.Declaration == nil || resolution.Declaration.Digest != prepared.Descriptor.Digest || len(resolution.Dependencies) != 2 {
			t.Fatalf("unexpected captured selection: %+v", resolution)
		}
		for _, dep := range resolution.Dependencies {
			if dep.Descriptor.Digest != image.Descriptor.Digest {
				t.Fatal("source tags did not share image content")
			}
		}
	}
	if selected != 1 {
		t.Fatalf("selected declarations=%d", selected)
	}
	storageRegistry.mu.Lock()
	var index v1.Index
	err = json.Unmarshal(storageRegistry.objects[created.Descriptor.Digest.String()], &index)
	storageRegistry.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	imageMembers := 0
	for _, member := range index.Manifests {
		if member.Digest == image.Descriptor.Digest {
			imageMembers++
		}
	}
	if imageMembers != 1 {
		t.Fatalf("shared image direct members=%d", imageMembers)
	}
	// A mutable source tag moves; the other alias still identifies the old graph.
	newImage := publishDependencyArtifact(t, client, imageHost, dependencyArtifact(t, "runtime-v2", true), "next")
	imageRegistry.mu.Lock()
	imageRegistry.tags["runtime"] = newImage.Descriptor.Digest.String()
	imageRegistry.mu.Unlock()
	latestLayout := filepath.Join(t.TempDir(), "latest")
	latest, err := client.Pull(ctx, bundleReference, latestLayout, bale.PullLatest)
	if err != nil || latest.Descriptor.Digest == created.Descriptor.Digest {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
	latestClient, _ := bale.NewClient(bale.ClientOptions{})
	placements := bale.PushOptions{RepositoryMap: map[string]string{subject.Repository: "application", image.Repository: "runtime"}}
	latestPlan, err := latestClient.PlanPush(ctx, latestLayout, "destination.example/release:latest", placements)
	if err != nil {
		t.Fatal(err)
	}
	latestTags := map[string]digest.Digest{}
	for _, tag := range latestPlan.Tags {
		latestTags[tag.Reference] = tag.Descriptor.Digest
	}
	if latestTags["runtime"] != newImage.Descriptor.Digest || latestTags["current"] != image.Descriptor.Digest {
		t.Fatalf("latest lost independent selectors: %+v", latestTags)
	}
	// Both original registries are now unavailable. A fresh client must pull
	// entirely from storage, then deliver the locked graph without provenance I/O.
	subjectServer.Close()
	imageServer.Close()
	lockedClient, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{storageHost}})
	if err != nil {
		t.Fatal(err)
	}
	lockedLayout := filepath.Join(t.TempDir(), "locked")
	locked, err := lockedClient.Pull(ctx, bundleReference, lockedLayout, bale.PullLocked)
	if err != nil || locked.Descriptor.Digest != created.Descriptor.Digest {
		t.Fatalf("locked=%+v err=%v", locked, err)
	}
	destination := newSDKRegistry()
	destinationServer := httptest.NewServer(destination)
	defer destinationServer.Close()
	destinationHost := strings.TrimPrefix(destinationServer.URL, "http://")
	targetClient, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{destinationHost}})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := targetClient.PlanPush(ctx, lockedLayout, destinationHost+"/release:complete", placements)
	if err != nil {
		t.Fatal(err)
	}
	destination.failAlias = "runtime"
	partial, err := targetClient.Push(ctx, delivery)
	if err == nil {
		t.Fatal("expected interrupted dependency alias publication")
	}
	var detail *bale.Error
	if !errors.As(err, &detail) || len(partial.Relocations) == 0 {
		t.Fatalf("missing confirmed partial result: %+v %v", partial, err)
	}
	destination.mu.Lock()
	_, premature := destination.tags["complete"]
	destination.failAlias = ""
	destination.mu.Unlock()
	if premature {
		t.Fatal("completion published before failed dependency tag")
	}
	result, err := targetClient.Push(ctx, delivery)
	if err != nil {
		t.Fatal(err)
	}
	relocated, ok := result.Relocations.Lookup(bale.ArtifactID{Repository: image.Repository, Digest: image.Descriptor.Digest})
	if !ok || relocated.Destination.Repository != delivery.RepositoryMap[image.Repository] {
		t.Fatalf("missing dependency relocation: %+v", result.Relocations)
	}
	destination.mu.Lock()
	if destination.tags["runtime"] != image.Descriptor.Digest.String() || destination.tags["current"] != image.Descriptor.Digest.String() {
		destination.mu.Unlock()
		t.Fatal("locked dependency tags changed")
	}
	if destination.tags["complete"] != created.Descriptor.Digest.String() {
		destination.mu.Unlock()
		t.Fatal("completion has wrong digest")
	}
	destination.mu.Unlock()
	// Check both platforms and their exact child bytes, not just index presence.
	storageRegistry.mu.Lock()
	imageBytes := append([]byte(nil), storageRegistry.objects[image.Descriptor.Digest.String()]...)
	storageRegistry.mu.Unlock()
	var imageIndex v1.Index
	if err := json.Unmarshal(imageBytes, &imageIndex); err != nil {
		t.Fatal(err)
	}
	if len(imageIndex.Manifests) != 2 {
		t.Fatal("lost an architecture")
	}
	destination.mu.Lock()
	defer destination.mu.Unlock()
	for _, manifest := range imageIndex.Manifests {
		if len(destination.objects[manifest.Digest.String()]) == 0 {
			t.Fatalf("lost platform %s", manifest.Platform.Architecture)
		}
	}
}

func TestPublicDependencyReadErrorsBeforePublication(t *testing.T) {
	ctx := context.Background()
	registry := newSDKRegistry()
	server := httptest.NewServer(registry)
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	subject := publishDependencyArtifact(t, client, host, dependencyArtifact(t, "subject", false), "application")
	prepared, err := bale.PrepareDependencyDeclaration(ctx, subject, bale.DependencyDeclaration{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Attach(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	raw, err := content.FetchAll(ctx, prepared.Store, prepared.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var declaration v1.Manifest
	if err := json.Unmarshal(raw, &declaration); err != nil {
		t.Fatal(err)
	}
	payload := declaration.Layers[0].Digest.String()
	recipe := bale.Recipe{Include: []bale.Selection{{Ref: subject.Repository + ":application", Referrers: bale.ReferrerPolicy{Include: "none"}}}}
	for _, mode := range []string{"denied", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			registry.mu.Lock()
			writes := registry.writes
			if mode == "denied" {
				registry.failRead = payload
			} else {
				registry.corruptRead = payload
			}
			registry.mu.Unlock()
			result, err := client.Create(ctx, recipe, host+"/bundle:"+mode, bale.CreateOptions{})
			if err == nil || result.Published {
				t.Fatalf("unexpected completion: %+v %v", result, err)
			}
			if errors.Is(err, bale.ErrIntegrity) != (mode == "corrupt") {
				t.Fatalf("incorrect error classification: %v", err)
			}
			registry.mu.Lock()
			unchanged := registry.writes == writes
			registry.failRead, registry.corruptRead = "", ""
			registry.mu.Unlock()
			if !unchanged {
				t.Fatal("capture failure wrote registry content")
			}
		})
	}
}
