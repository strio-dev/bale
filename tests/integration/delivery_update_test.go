package integration_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content/oci"
)

func TestPublicDeliveryUpdatePreservesPathsAcrossLatestRefresh(t *testing.T) {
	ctx := t.Context()
	sourceRegistry, holdingRegistry := newSDKRegistry(), newSDKRegistry()
	source, holding := httptest.NewServer(sourceRegistry), httptest.NewServer(holdingRegistry)
	defer source.Close()
	defer holding.Close()
	sourceHost, holdingHost := strings.TrimPrefix(source.URL, "http://"), strings.TrimPrefix(holding.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{sourceHost, holdingHost}})
	if err != nil {
		t.Fatal(err)
	}
	subject := publishDependencyArtifact(t, client, sourceHost, dependencyArtifact(t, "update-subject", false), "application")
	oldImage := publishDependencyArtifact(t, client, sourceHost, dependencyArtifact(t, "update-runtime-v1", true), "runtime")
	prepared, err := bale.PrepareDependencyDeclaration(ctx, subject, bale.DependencyDeclaration{Dependencies: []bale.DependencyReference{{Ref: oldImage.Repository + ":runtime"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Attach(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	recipe := bale.Recipe{Include: []bale.Selection{{Ref: subject.Repository + ":application"}}}
	beforeRef, afterRef := holdingHost+"/bundles:before", holdingHost+"/bundles:after"
	before, err := client.Create(ctx, recipe, beforeRef, bale.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lockedPath, latestPath := filepath.Join(t.TempDir(), "locked"), filepath.Join(t.TempDir(), "latest")
	if _, err := client.Pull(ctx, beforeRef, lockedPath, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	file, err := client.ExportDeliveryLayout(ctx, lockedPath)
	if err != nil {
		t.Fatal(err)
	}
	oldID := oldImage.Repository + "@" + oldImage.Descriptor.Digest.String()
	found := false
	for i := range file.Artifacts {
		if file.Artifacts[i].Ref == oldID {
			file.Artifacts[i].Path = "curated/runtime"
			found = true
		}
	}
	if !found {
		t.Fatal("export omitted dependency root")
	}
	originalFile, err := bale.EncodeDeliveryFile(file)
	if err != nil {
		t.Fatal(err)
	}
	newImage := publishDependencyArtifact(t, client, sourceHost, dependencyArtifact(t, "update-runtime-v2", true), "next")
	sourceRegistry.mu.Lock()
	sourceRegistry.tags["runtime"] = newImage.Descriptor.Digest.String()
	sourceRegistry.mu.Unlock()
	latest, err := client.Pull(ctx, beforeRef, latestPath, bale.PullLatest)
	if err != nil {
		t.Fatal(err)
	}
	after, err := client.Create(ctx, recipe, afterRef, bale.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if before.Descriptor.Digest == latest.Descriptor.Digest || after.Descriptor.Digest != latest.Descriptor.Digest {
		t.Fatal("dependency tag movement did not produce the expected refreshed snapshot")
	}
	source.Close()
	beforeFiles, afterFiles := inspectLayoutFiles(t, lockedPath), inspectLayoutFiles(t, latestPath)
	opaque := map[string]bool{}
	for _, value := range []string{"opaque component config", "update-subject", "update-runtime-v1", "update-runtime-v2"} {
		opaque[digest.FromString(value).String()] = true
	}
	var mu sync.Mutex
	requests, forbidden, pluginCalls := 0, 0, 0
	reader, err := bale.NewClient(bale.ClientOptions{
		PlainHTTPRegistries: []string{holdingHost},
		PluginRunner: func(context.Context, bale.PluginRequest) (bale.DependencyDeclaration, error) {
			mu.Lock()
			pluginCalls++
			mu.Unlock()
			return bale.DependencyDeclaration{}, errors.New("delivery update executed a plugin")
		},
		HTTPClient: &http.Client{Transport: inspectRoundTripper(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			requests++
			key := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
			if req.URL.Host != holdingHost || !strings.HasPrefix(req.URL.Path, "/v2/bundles/") || (req.Method != http.MethodGet && req.Method != http.MethodHead) || strings.Contains(req.URL.Path, "/referrers/") || opaque[key] {
				forbidden++
				return nil, errors.New("delivery update escaped captured metadata")
			}
			return http.DefaultTransport.RoundTrip(req)
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	holdingRegistry.mu.Lock()
	writes := holdingRegistry.writes
	holdingRegistry.mu.Unlock()
	remote, err := reader.UpdateDelivery(ctx, beforeRef, afterRef, file)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	remoteReads := requests
	mu.Unlock()
	local, err := reader.UpdateDeliveryLayouts(ctx, lockedPath, latestPath, file)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := reader.UpdateDeliveryRoots(ctx,
		bale.Root{Descriptor: before.Descriptor, Store: oci.NewStorageFromFS(os.DirFS(lockedPath))},
		bale.Root{Descriptor: latest.Descriptor, Store: oci.NewStorageFromFS(os.DirFS(latestPath))}, file)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range []bale.DeliveryUpdateResult{local, pinned} {
		if got.Before.Digest != remote.Before.Digest || got.After.Digest != remote.After.Digest {
			t.Fatal("update entry points disagree on snapshot identity")
		}
		// Registry resolution may omit optional descriptor decorations.
		got.Before, got.After = remote.Before, remote.After
		if !reflect.DeepEqual(remote, got) {
			t.Fatalf("update entry points disagree: remote=%+v local=%+v", remote, got)
		}
	}
	if local.Delivery == nil || len(local.Conflicts) != 0 || local.Delivery.Bundle != latest.Descriptor.Digest {
		t.Fatalf("update did not bind a usable delivery file to the new snapshot: %+v", local)
	}
	newID := newImage.Repository + "@" + newImage.Descriptor.Digest.String()
	paths := map[string]string{}
	for _, row := range local.Delivery.Artifacts {
		paths[row.Ref] = row.Path
	}
	if paths[newID] != "curated/runtime" || paths[oldID] != "" {
		t.Fatalf("lost edited dependency placement: %+v", paths)
	}
	// Only the tagged root has evidence connecting its old and new identity.
	// Untagged platform children disappear or receive ordinary default paths.
	moved, removed, defaulted := 0, 0, 0
	for _, change := range local.Changes {
		switch change.Change {
		case "moved":
			moved++
			if change.Before != oldID || change.After != newID || change.Path != "curated/runtime" {
				t.Fatalf("invented selector pairing: %+v", change)
			}
		case "removed":
			removed++
			if _, exists := paths[change.Before]; exists {
				t.Fatalf("removed platform child survived: %+v", change)
			}
		case "defaulted":
			defaulted++
			if change.Path != "source/component" {
				t.Fatalf("new platform child inherited a root override: %+v", change)
			}
		}
	}
	if moved != 1 || removed != 2 || defaulted != 2 {
		t.Fatalf("unexpected multiarch update changes: %+v", local.Changes)
	}
	plan, err := reader.PlanPush(ctx, latestPath, "destination.example/release:bundle", bale.PushOptions{Delivery: local.Delivery})
	if err != nil {
		t.Fatal(err)
	}
	placement, ok := plan.Relocations.Lookup(bale.ArtifactID{Repository: newImage.Repository, Digest: newImage.Descriptor.Digest})
	if !ok || placement.Destination.Repository != "destination.example/release/curated/runtime" || plan.Descriptor.Digest != latest.Descriptor.Digest {
		t.Fatalf("updated paths did not reach the new delivery plan: %+v", plan)
	}
	restored := false
	for _, tag := range plan.Tags {
		if tag.Reference == "runtime" && tag.Repository == placement.Destination.Repository && tag.Descriptor.Digest == newImage.Descriptor.Digest {
			restored = true
		}
	}
	if !restored {
		t.Fatal("new runtime tag was not planned in the edited canonical repository")
	}
	encoded, err := bale.EncodeDeliveryFile(file)
	if err != nil || string(encoded) != string(originalFile) || !reflect.DeepEqual(beforeFiles, inspectLayoutFiles(t, lockedPath)) || !reflect.DeepEqual(afterFiles, inspectLayoutFiles(t, latestPath)) {
		t.Fatal("delivery update or planning mutated its inputs")
	}
	mu.Lock()
	defer mu.Unlock()
	if requests == 0 || requests != remoteReads || forbidden != 0 || pluginCalls != 0 {
		t.Fatalf("unexpected update/planning I/O: reads=%d remote=%d forbidden=%d plugins=%d", requests, remoteReads, forbidden, pluginCalls)
	}
	holdingRegistry.mu.Lock()
	defer holdingRegistry.mu.Unlock()
	if writes != holdingRegistry.writes {
		t.Fatal("delivery update published registry content")
	}
}
