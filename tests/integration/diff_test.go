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

func TestPublicDiffReviewsLatestDependencyRefreshOffline(t *testing.T) {
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
	subject := publishDependencyArtifact(t, client, sourceHost, dependencyArtifact(t, "diff-subject", false), "application")
	oldImage := publishDependencyArtifact(t, client, sourceHost, dependencyArtifact(t, "diff-runtime-v1", true), "runtime")
	dependencyRef := oldImage.Repository + ":runtime"
	declaration, err := bale.PrepareDependencyDeclaration(ctx, subject, bale.DependencyDeclaration{Dependencies: []bale.DependencyReference{{Ref: dependencyRef}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Attach(ctx, declaration); err != nil {
		t.Fatal(err)
	}
	recipe := bale.Recipe{Include: []bale.Selection{{Ref: subject.Repository + ":application"}}}
	beforeRef, afterRef := holdingHost+"/bundles:before", holdingHost+"/bundles:after"
	before, err := client.Create(ctx, recipe, beforeRef, bale.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lockedPath := filepath.Join(t.TempDir(), "locked")
	if _, err := client.Pull(ctx, beforeRef, lockedPath, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	newImage := publishDependencyArtifact(t, client, sourceHost, dependencyArtifact(t, "diff-runtime-v2", true), "next")
	sourceRegistry.mu.Lock()
	sourceRegistry.tags["runtime"] = newImage.Descriptor.Digest.String()
	sourceRegistry.mu.Unlock()
	latestPath := filepath.Join(t.TempDir(), "latest")
	latest, err := client.Pull(ctx, beforeRef, latestPath, bale.PullLatest)
	if err != nil {
		t.Fatal(err)
	}
	after, err := client.Create(ctx, recipe, afterRef, bale.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if latest.Descriptor.Digest != after.Descriptor.Digest || before.Descriptor.Digest == after.Descriptor.Digest {
		t.Fatalf("unexpected captures: before=%s latest=%s after=%s", before.Descriptor.Digest, latest.Descriptor.Digest, after.Descriptor.Digest)
	}
	// All source selectors are now unreachable. Comparison must read only pinned
	// manifests and interpreted JSON in the holding registry or the local layouts.
	source.Close()
	opaque := map[string]bool{}
	for _, value := range []string{"opaque component config", "diff-subject", "diff-runtime-v1", "diff-runtime-v2"} {
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
			return bale.DependencyDeclaration{}, errors.New("diff executed a plugin")
		},
		HTTPClient: &http.Client{Transport: inspectRoundTripper(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			requests++
			key := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
			if req.URL.Host != holdingHost || !strings.HasPrefix(req.URL.Path, "/v2/bundles/") || (req.Method != http.MethodGet && req.Method != http.MethodHead) || strings.Contains(req.URL.Path, "/referrers/") || opaque[key] {
				forbidden++
				return nil, errors.New("diff escaped captured metadata")
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
	remote, err := reader.Diff(ctx, beforeRef, afterRef)
	if err != nil {
		t.Fatal(err)
	}
	if !remote.Changed || remote.RecipeChanged || remote.Before.Digest != before.Descriptor.Digest || remote.After.Digest != latest.Descriptor.Digest {
		t.Fatalf("incorrect comparison: %+v", remote)
	}
	if len(remote.Resolutions) != 1 {
		t.Fatalf("resolution changes: %+v", remote.Resolutions)
	}
	change := remote.Resolutions[0]
	if change.Kind != "dependency" || change.Ref != dependencyRef || len(change.Before) != 1 || len(change.After) != 1 || change.Before[0].Digest != oldImage.Descriptor.Digest || change.After[0].Digest != newImage.Descriptor.Digest {
		t.Fatalf("lost tag movement: %+v", change)
	}
	membership := map[string]bool{}
	for _, artifact := range remote.Artifacts {
		membership[artifact.Change+" "+artifact.Repository+"@"+artifact.Descriptor.Digest.String()] = true
	}
	if !membership["removed "+oldImage.Repository+"@"+oldImage.Descriptor.Digest.String()] || !membership["added "+newImage.Repository+"@"+newImage.Descriptor.Digest.String()] {
		t.Fatalf("lost artifact membership changes: %+v", remote.Artifacts)
	}
	mu.Lock()
	remoteReads := requests
	mu.Unlock()
	// Removing opaque bytes demonstrates the explicit metadata-only scope of all
	// diff entry points. This is deliberately not a transfer-integrity check.
	for _, path := range []string{lockedPath, latestPath} {
		for value := range opaque {
			d := digest.Digest(value)
			err := os.Remove(filepath.Join(path, "blobs", d.Algorithm().String(), d.Encoded()))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
	}
	local, err := reader.DiffLayouts(ctx, lockedPath, latestPath)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := reader.DiffRoots(ctx,
		bale.Root{Descriptor: before.Descriptor, Store: oci.NewStorageFromFS(os.DirFS(lockedPath))},
		bale.Root{Descriptor: latest.Descriptor, Store: oci.NewStorageFromFS(os.DirFS(latestPath))})
	if err != nil {
		t.Fatal(err)
	}
	// Registry resolution need not return optional descriptor decorations such
	// as artifactType, while the captured layout retains them.
	for _, got := range []bale.DiffResult{local, pinned} {
		if got.Before.Digest != remote.Before.Digest || got.Before.Size != remote.Before.Size || got.Before.MediaType != remote.Before.MediaType || got.After.Digest != remote.After.Digest || got.After.Size != remote.After.Size || got.After.MediaType != remote.After.MediaType {
			t.Fatalf("entry points disagree on pinned identity: %+v", got)
		}
		got.Before, got.After = remote.Before, remote.After
		if !reflect.DeepEqual(remote, got) {
			t.Fatalf("entry points disagree on changes:\nremote=%+v\nlocal=%+v", remote, got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if requests == 0 || requests != remoteReads || forbidden != 0 || pluginCalls != 0 {
		t.Fatalf("comparison side effects: reads=%d forbidden=%d plugins=%d", requests, forbidden, pluginCalls)
	}
	holdingRegistry.mu.Lock()
	defer holdingRegistry.mu.Unlock()
	if writes != holdingRegistry.writes {
		t.Fatal("diff published registry content")
	}
}
