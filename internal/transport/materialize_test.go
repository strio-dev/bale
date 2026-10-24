package transport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"encoding/json"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content/oci"
)

func TestCreateIntoSharedCASAndPinnedHydration(t *testing.T) {
	r, repo, service, writes := monitoredRegistry(t)
	member := object(t, r.store, "shared opaque payload")
	r.tags["one"], r.tags["two"] = member, member
	cas, err := oci.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	create := func(tag string) (CreateResult, *CaptureState) {
		t.Helper()
		recipe := config.Recipe{Include: []config.Selection{dependencySelection(repo + ":" + tag)}}
		result, state, err := service.CreateInto(ctx, recipe, cas, CreateOptions{ExportDelivery: true})
		if err != nil || state == nil || !result.MetadataOnly || result.Published || result.LayoutPath != "" || result.Delivery == nil {
			t.Fatalf("capture into shared storage: %+v, state=%+v, error=%v", result, state, err)
		}
		return result, state
	}
	first, firstState := create("one")
	second, secondState := create("two")
	if first.Descriptor.Digest == second.Descriptor.Digest || r.blobReads != 0 || writes.Load() != 0 {
		t.Fatalf("captured identities or lazy behavior lost: blob GETs=%d registry writes=%d", r.blobReads, writes.Load())
	}
	firstSnapshot, inventory, err := bundle.InspectSnapshotInventory(ctx, cas, first.Descriptor)
	if err != nil || len(inventory) == 0 {
		t.Fatalf("metadata inventory: %v", err)
	}
	if _, err := bundle.LoadSnapshotGraph(ctx, cas, first.Descriptor); err == nil {
		t.Fatal("metadata-only shared capture unexpectedly complete")
	}
	var payload v1.Descriptor
	for dgst := range firstState.Sources {
		payload = inventory[dgst]
		if present, err := cas.Exists(ctx, payload); err != nil || present {
			t.Fatalf("missing payload was eagerly retained: %t %v", present, err)
		}
	}
	resolutions := r.headTags["one"] + r.headTags["two"]
	r.tags["one"], r.tags["two"] = object(t, r.store, "new payload"), object(t, r.store, "other payload")
	hydrated, err := service.MaterializeInto(ctx, Root{Descriptor: first.Descriptor, Store: cas}, firstState, cas)
	if err != nil || hydrated.Descriptor.Digest != first.Descriptor.Digest || hydrated.Stats.BlobBytesDownloaded != payload.Size || hydrated.Stats.BlobBytesUploaded != 0 {
		t.Fatalf("pinned hydration: %+v %v", hydrated, err)
	}
	if r.headTags["one"]+r.headTags["two"] != resolutions || writes.Load() != 0 {
		t.Fatal("materialization resolved a tag or wrote to source registry")
	}
	if err := ValidateCaptureState(ctx, cas, firstSnapshot, *firstState); err != nil {
		t.Fatalf("hydrated capture locator rejected: %v", err)
	}
	secondSnapshot, err := bundle.LoadSnapshotGraph(ctx, cas, second.Descriptor)
	if err != nil {
		t.Fatalf("one shared hydration did not complete both bundles: %v", err)
	}
	if err := ValidateCaptureState(ctx, cas, secondSnapshot, *secondState); err != nil {
		t.Fatalf("shared hydration invalidated other capture: %v", err)
	}
	before := r.blobReads
	delete(r.store.descriptors, payload.Digest.String())
	reused, err := service.MaterializeInto(ctx, Root{Descriptor: second.Descriptor, Store: cas}, secondState, cas)
	if err != nil || reused.Stats.BlobBytesDownloaded != 0 || r.blobReads != before {
		t.Fatalf("shared retained payload was downloaded again: %+v %v", reused, err)
	}
}

func TestCreateIntoDryRunAndFailedRetention(t *testing.T) {
	r, repo, service, _ := monitoredRegistry(t)
	r.tags["stable"] = object(t, r.store, "payload")
	recipe := config.Recipe{Include: []config.Selection{dependencySelection(repo + ":stable")}}
	cas, err := oci.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	preview, state, err := service.CreateInto(ctx, recipe, cas, CreateOptions{DryRun: true, ExportDelivery: true})
	if err != nil || state != nil || !preview.DryRun || preview.Delivery == nil || preview.Published {
		t.Fatalf("dry run: %+v %+v %v", preview, state, err)
	}
	if exists, err := cas.Exists(ctx, preview.Descriptor); err != nil || exists {
		t.Fatalf("dry run wrote capture root: %t %v", exists, err)
	}
	failed, state, err := service.CreateInto(ctx, recipe, &failStorage{Storage: cas, remaining: 1}, CreateOptions{})
	if err == nil || state != nil || failed.Published || failed.LayoutPath != "" {
		t.Fatalf("failed retention reported completion: %+v %+v %v", failed, state, err)
	}
	if _, _, err := service.CreateInto(ctx, recipe, nil, CreateOptions{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil target accepted: %v", err)
	}
}

func TestMaterializeIntoRejectsCancellationAndBadPayload(t *testing.T) {
	for _, scenario := range []string{"canceled", "missing", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			r, repo, service, _ := monitoredRegistry(t)
			r.tags["stable"] = object(t, r.store, "payload")
			directory := t.TempDir()
			cas, err := oci.NewStorage(directory)
			if err != nil {
				t.Fatal(err)
			}
			created, state, err := service.CreateInto(ctx, config.Recipe{Include: []config.Selection{dependencySelection(repo + ":stable")}}, cas, CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			operationCtx := ctx
			if scenario == "canceled" {
				var cancel context.CancelFunc
				operationCtx, cancel = context.WithCancel(ctx)
				cancel()
			} else {
				for dgst := range state.Sources {
					if scenario == "missing" {
						delete(r.store.descriptors, dgst.String())
					} else {
						// Shared CAS damage must fail full verification rather than
						// silently repairing from a recorded source.
						path := filepath.Join(directory, "blobs", dgst.Algorithm().String(), dgst.Encoded())
						if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			result, err := service.MaterializeInto(operationCtx, Root{Descriptor: created.Descriptor, Store: cas}, state, cas)
			if err == nil {
				t.Fatal("incomplete, corrupt or canceled materialization succeeded")
			}
			if scenario == "canceled" && (!errors.Is(err, context.Canceled) || r.blobReads != 0) {
				t.Fatalf("cancellation reached payload: reads=%d error=%v", r.blobReads, err)
			}
			if scenario == "corrupt" && r.blobReads != 0 {
				t.Fatal("local corruption triggered remote repair")
			}
			if result.LayoutPath != "" {
				t.Fatal("CAS materialization reported a filesystem layout")
			}
		})
	}
}

func TestPullIntoRetainsLockedAndLatestBundles(t *testing.T) {
	r, repo, service, writes := monitoredRegistry(t)
	old := object(t, r.store, "old payload")
	r.tags["stable"] = old
	stored := snapshotFor(t, r.store, []string{repo}, []v1.Descriptor{old})
	r.tags["bundle"] = stored.Descriptor
	cas, err := oci.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	locked, err := service.PullInto(ctx, repo+":bundle", cas, PullLocked, PullOptions{})
	if err != nil || locked.Descriptor.Digest != stored.Descriptor.Digest || locked.LayoutPath != "" || locked.Stats.BlobBytesDownloaded == 0 || locked.Stats.BlobBytesUploaded != 0 {
		t.Fatalf("locked pull into CAS: %+v %v", locked, err)
	}
	before := r.blobReads
	reused, err := service.PullInto(ctx, repo+":bundle", cas, PullLocked, PullOptions{})
	if err != nil || reused.Stats.BlobBytesDownloaded != 0 || r.blobReads != before {
		t.Fatalf("locked pull reread existing payload: %+v %v", reused, err)
	}
	fresh := object(t, r.store, "fresh payload")
	r.tags["stable"] = fresh
	latest, err := service.PullInto(ctx, repo+":bundle", cas, PullLatest, PullOptions{})
	if err != nil || latest.Descriptor.Digest == locked.Descriptor.Digest {
		t.Fatalf("latest pull did not refresh metadata: %+v %v", latest, err)
	}
	snapshot, err := bundle.LoadSnapshotGraph(ctx, cas, latest.Descriptor)
	if err != nil || snapshot.Metadata.Roots[0].Descriptor.Digest != fresh.Digest {
		t.Fatalf("latest CAS capture not complete: %+v %v", snapshot, err)
	}
	if writes.Load() != 0 {
		t.Fatal("CAS pull published into source registry")
	}
	if _, err := service.PullInto(ctx, repo+":bundle", cas, "invalid", PullOptions{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid pull mode accepted: %v", err)
	}
}

func TestPullIntoLatestRequiresPluginAuthorization(t *testing.T) {
	r, repo, service, _ := monitoredRegistry(t)
	r.tags["subject"] = object(t, r.store, "subject")
	r.tags["dependency"] = object(t, r.store, "dependency")
	capturePluginFixture(t, r)
	calls := 0
	service.pluginRunner = func(context.Context, PluginRequest) (dependency.Declaration, error) {
		calls++
		return dependency.Declaration{Dependencies: []dependency.Reference{{Ref: repo + ":dependency"}}}, nil
	}
	selected := dependencySelection(repo + ":subject")
	selected.Plugin = &config.Plugin{Ref: repo + ":plugin", Input: json.RawMessage(`{}`)}
	captured, snapshot, err := service.capture(ctx, config.Recipe{Include: []config.Selection{selected}})
	if err != nil {
		t.Fatal(err)
	}
	if err := copyGraph(ctx, captured, r.store, snapshot.Descriptor, &Stats{}, nil); err != nil {
		t.Fatal(err)
	}
	r.tags["bundle"] = snapshot.Descriptor
	calls = 0
	cas, err := oci.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PullInto(ctx, repo+":bundle", cas, PullLatest, PullOptions{}); !errors.Is(err, ErrInvalid) || calls != 0 {
		t.Fatalf("latest executed unauthorized plugin: calls=%d error=%v", calls, err)
	}
	result, err := service.PullInto(ctx, repo+":bundle", cas, PullLatest, PullOptions{AllowPlugins: true})
	if err != nil || calls != 1 {
		t.Fatalf("authorized latest failed: calls=%d error=%v", calls, err)
	}
	if _, err := bundle.LoadSnapshotGraph(ctx, cas, result.Descriptor); err != nil {
		t.Fatalf("authorized latest retained incomplete bundle: %v", err)
	}
}
