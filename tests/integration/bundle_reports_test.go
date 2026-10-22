package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"net/http/httptest"
	"oras.land/oras-go/v2/content"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strio-dev/bale"
)

func TestBundlePublicationPartialReportsAndRetry(t *testing.T) {
	sourceRegistry := newSDKRegistry()
	sourceServer := httptest.NewServer(sourceRegistry)
	defer sourceServer.Close()
	sourceHost := strings.TrimPrefix(sourceServer.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{sourceHost}})
	if err != nil {
		t.Fatal(err)
	}
	root := sdkFixture(t)
	plan, err := client.Plan(context.Background(), bale.TransferRequest{Roots: []bale.Root{root}, Destination: sourceHost + "/source"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Execute(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	recipe := bale.Recipe{Include: []bale.Selection{
		{Ref: sourceHost + "/source/component:a", Referrers: bale.ReferrerPolicy{Include: "none"}},
		{Ref: sourceHost + "/source/component:b", Referrers: bale.ReferrerPolicy{Include: "none"}},
	}}
	sourceRegistry.mu.Lock()
	sourceRegistry.failAlias = "bundle"
	sourceRegistry.mu.Unlock()
	created, err := client.Create(context.Background(), recipe, sourceHost+"/storage:bundle", bale.CreateOptions{})
	if err == nil || created.Published || created.Stats.ManifestBytesUploaded == 0 {
		t.Fatalf("untruthful partial create: %+v err=%v", created, err)
	}
	sourceRegistry.mu.Lock()
	sourceRegistry.failAlias = ""
	sourceRegistry.mu.Unlock()
	created, err = client.Create(context.Background(), recipe, sourceHost+"/storage:bundle", bale.CreateOptions{})
	if err != nil || !created.Published {
		t.Fatalf("create retry: %+v err=%v", created, err)
	}
	layout := filepath.Join(t.TempDir(), "layout")
	if _, err := client.Pull(context.Background(), sourceHost+"/storage:bundle", layout, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"b", root.Descriptor.Digest.String()} {
		t.Run(failure, func(t *testing.T) {
			destination := newSDKRegistry()
			destination.failAlias = failure
			server := httptest.NewServer(destination)
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "http://")
			var events int
			targetClient, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}, OnProgress: func(bale.Progress) { events++ }})
			if err != nil {
				t.Fatal(err)
			}
			delivery, err := targetClient.PlanPush(context.Background(), layout, host+"/delivery:complete", bale.PushOptions{})
			if err != nil {
				t.Fatal(err)
			}
			result, err := targetClient.Push(context.Background(), delivery)
			var detail *bale.Error
			if err == nil || !errors.As(err, &detail) || result.Stats.BlobBytesUploaded == 0 {
				t.Fatalf("missing partial upload report: %+v err=%v", result, err)
			}
			if failure == "b" {
				if len(result.Tags) != 1 || result.Tags[0].Reference != "a" || len(result.Relocations) == 0 {
					t.Fatalf("untruthful alias failure result: %+v", result)
				}
			} else if len(result.Tags) != 0 || len(result.Relocations) != 0 {
				t.Fatalf("unverified graph reported complete: %+v", result)
			}
			destination.mu.Lock()
			_, complete := destination.tags["complete"]
			destination.failAlias = ""
			destination.mu.Unlock()
			if complete {
				t.Fatal("completion tag published before failed operation completed")
			}
			result, err = targetClient.Push(context.Background(), delivery)
			if err != nil || len(result.Tags) != 2 || len(result.Relocations) == 0 || events == 0 {
				t.Fatalf("bundle retry result=%+v err=%v events=%d", result, err, events)
			}
			destination.mu.Lock()
			_, complete = destination.tags["complete"]
			destination.mu.Unlock()
			if !complete {
				t.Fatal("completion absent after retry")
			}
		})
	}
}

func TestBundlePullTypedErrorsAndPartialStats(t *testing.T) {
	registry := newSDKRegistry()
	server := httptest.NewServer(registry)
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	root := sdkFixture(t)
	plan, err := client.Plan(context.Background(), bale.TransferRequest{Roots: []bale.Root{root}, Destination: host + "/source"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Execute(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	recipe := bale.Recipe{Include: []bale.Selection{{Ref: host + "/source/component:a", Referrers: bale.ReferrerPolicy{Include: "none"}}}}
	if _, err := client.Create(context.Background(), recipe, host+"/storage:bundle", bale.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ref := host + "/storage:bundle"
	if _, err := client.Pull(context.Background(), ref, filepath.Join(t.TempDir(), "badmode"), bale.PullMode("bad")); !errors.Is(err, bale.ErrInvalid) {
		t.Fatalf("invalid mode: %v", err)
	}
	if _, err := client.Pull(context.Background(), ref, t.TempDir(), bale.PullLocked); !errors.Is(err, bale.ErrConflict) {
		t.Fatalf("existing path: %v", err)
	}
	if _, err := client.Pull(context.Background(), host+"/storage:absent", filepath.Join(t.TempDir(), "missing"), bale.PullLocked); !errors.Is(err, bale.ErrNotFound) {
		t.Fatalf("missing source: %v", err)
	}
	raw, err := content.FetchAll(context.Background(), root.Store, root.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	payload := manifest.Layers[0].Digest.String()
	for _, mode := range []string{"missing", "corrupt", "denied"} {
		t.Run(mode, func(t *testing.T) {
			registry.mu.Lock()
			saved := registry.objects[payload]
			switch mode {
			case "missing":
				delete(registry.objects, payload)
			case "corrupt":
				registry.corruptRead = payload
			case "denied":
				registry.failRead = payload
			}
			registry.mu.Unlock()
			parent := t.TempDir()
			path := filepath.Join(parent, "layout")
			result, err := client.Pull(context.Background(), ref, path, bale.PullLocked)
			if err == nil || result.LayoutPath != "" || result.Stats.BlobBytesDownloaded == 0 || result.Stats.BlobBytesUploaded != 0 {
				t.Fatalf("partial pull: %+v err=%v", result, err)
			}
			switch mode {
			case "missing":
				if !errors.Is(err, bale.ErrNotFound) {
					t.Fatalf("missing payload: %v", err)
				}
			case "corrupt":
				if !errors.Is(err, bale.ErrIntegrity) {
					t.Fatalf("corrupt payload: %v", err)
				}
			case "denied":
				if errors.Is(err, bale.ErrIntegrity) {
					t.Fatalf("read denial classified corruption: %v", err)
				}
			}
			entries, readErr := os.ReadDir(parent)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("temporary layout leaked: %v %v", entries, readErr)
			}
			registry.mu.Lock()
			registry.objects[payload] = saved
			registry.corruptRead = ""
			registry.failRead = ""
			registry.mu.Unlock()
			if result, err := client.Pull(context.Background(), ref, path, bale.PullLocked); err != nil || result.LayoutPath != path {
				t.Fatalf("retry: %+v %v", result, err)
			}
		})
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if result, err := client.Pull(canceled, ref, filepath.Join(t.TempDir(), "canceled"), bale.PullLocked); !errors.Is(err, context.Canceled) || result.LayoutPath != "" {
		t.Fatalf("canceled pull: %+v %v", result, err)
	}
}
