package store_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/store"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
)

// deliveryRepository keeps each in-memory repository isolated. These fixtures
// use no external registry or credential configuration.
type deliveryRepository struct{ *memory.Store }

func (r deliveryRepository) Blobs() bale.RegistryBlobStore { return r }

func (r deliveryRepository) Push(ctx context.Context, desc v1.Descriptor, reader io.Reader) error {
	if err := r.Store.Push(ctx, desc, reader); err != nil {
		return err
	}
	return r.Tag(ctx, desc, desc.Digest.String())
}

func (deliveryRepository) ReferrerPages(context.Context, v1.Descriptor, func(bale.ReferrerPage) error) error {
	return errdef.ErrUnsupported
}

func TestManagedDeliveryPinsSelectedTags(t *testing.T) {
	const (
		sourceName   = "source.invalid/app"
		mirror       = "mirror.invalid/release"
		mirrorMember = mirror + "/selected"
		offline      = "offline.invalid/release"
	)
	ctx := t.Context()
	f := &fixture{store: memory.New(), descriptors: map[digest.Digest]v1.Descriptor{}}
	f.payload = f.put(t, "application/example", []byte("payload"))
	chosen, excluded := f.artifact(t, "chosen"), f.artifact(t, "excluded")
	// memory.Store.Resolve needs explicit references even for a pushed digest.
	for id, desc := range f.descriptors {
		if err := f.store.Tag(ctx, desc, id.String()); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Tag(ctx, chosen, "stable"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Tag(ctx, excluded, "experimental"); err != nil {
		t.Fatal(err)
	}

	repositories := map[string]deliveryRepository{
		sourceName:            {f.store},
		mirror:                {memory.New()},
		mirrorMember:          {memory.New()},
		offline:               {memory.New()},
		offline + "/selected": {memory.New()},
	}
	client, err := bale.NewClient(bale.ClientOptions{
		RepositoryFactory: func(_ context.Context, name string) (bale.RegistryRepository, error) {
			repository, ok := repositories[name]
			if !ok {
				return nil, fmt.Errorf("unexpected repository access: %s", name)
			}
			return repository, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	recipe, err := bale.ParseRecipe([]byte(`defaults:
  dependencies:
    follow: none
  referrers:
    include: none
include:
  - repository: source.invalid/app
    tags: [stable]
`))
	if err != nil {
		t.Fatal(err)
	}
	s := local(t, client)
	captured, err := s.Create(ctx, recipe, store.CreateOptions{Tags: []string{"release:v1"}})
	if err != nil || !captured.MetadataOnly {
		t.Fatalf("metadata capture: digest=%s metadataOnly=%v err=%v", captured.Descriptor.Digest, captured.MetadataOnly, err)
	}
	id := captured.Descriptor.Digest.String()
	// The metadata-only export and plan must work with no source lookup or
	// payload download. The source becomes available again for the actual push.
	delete(repositories, sourceName)
	file, err := s.ExportDelivery(ctx, id)
	if err != nil || file.Bundle != captured.Descriptor.Digest || len(file.Artifacts) != 1 || file.Artifacts[0].Ref != sourceName+"@"+chosen.Digest.String() {
		t.Fatalf("delivery must include only the selected artifact: %+v %v", file, err)
	}
	file.Artifacts[0].Path = "selected"
	options := bale.PushOptions{Delivery: &file}

	// A source tag change after capture must not change the local plan or what
	// is uploaded; the delivered alias still points at the captured digest.
	if err := f.store.Tag(ctx, excluded, "stable"); err != nil {
		t.Fatal(err)
	}
	plan, err := s.PlanPush(ctx, id, mirror+":complete", options)
	if err != nil || !plan.MetadataOnly || plan.LayoutPath != id || plan.Descriptor.Digest != captured.Descriptor.Digest {
		t.Fatalf("plan by immutable ID: digest=%s metadataOnly=%v reference=%q err=%v", plan.Descriptor.Digest, plan.MetadataOnly, plan.LayoutPath, err)
	}
	if len(plan.Tags) != 1 || plan.Tags[0].Repository != mirrorMember || plan.Tags[0].Reference != "stable" || plan.Tags[0].Descriptor.Digest != chosen.Digest {
		t.Fatalf("unexpected tags for selected artifact: %+v", plan.Tags)
	}
	repositories[sourceName] = deliveryRepository{f.store}
	pushed, err := s.Push(ctx, "release:v1", mirror+":complete", options)
	if err != nil || pushed.Descriptor.Digest != captured.Descriptor.Digest {
		t.Fatalf("push metadata-only capture: %s %v", pushed.Descriptor.Digest, err)
	}
	member, err := client.Resolve(ctx, mirrorMember+":stable")
	if err != nil || member.Descriptor.Digest != chosen.Digest {
		t.Fatalf("delivered alias lost its pin: %s %v", member.Descriptor.Digest, err)
	}
	if _, err := client.Resolve(ctx, mirrorMember+":experimental"); !errors.Is(err, bale.ErrNotFound) {
		t.Fatalf("unselected tag unexpectedly delivered: %v", err)
	}

	// Pull and push the completed root by digest with both previous registries
	// unavailable. Only the offline destination repositories remain accessible.
	offlineStore := local(t, client)
	pulled, err := offlineStore.Pull(ctx, mirror+":complete", store.PullOptions{})
	if err != nil || pulled.Mode != bale.PullLocked || pulled.Descriptor.Digest != captured.Descriptor.Digest {
		t.Fatalf("pull delivered graph: mode=%s digest=%s err=%v", pulled.Mode, pulled.Descriptor.Digest, err)
	}
	delete(repositories, sourceName)
	delete(repositories, mirror)
	delete(repositories, mirrorMember)
	offlinePlan, err := offlineStore.PlanPush(ctx, id, offline+":complete", options)
	if err != nil || offlinePlan.MetadataOnly || offlinePlan.LayoutPath != id {
		t.Fatalf("plan complete root offline: metadataOnly=%v reference=%q err=%v", offlinePlan.MetadataOnly, offlinePlan.LayoutPath, err)
	}
	if _, err := offlineStore.Push(ctx, id, offline+":complete", options); err != nil {
		t.Fatalf("push complete root offline: %v", err)
	}
	complete, err := client.Resolve(ctx, offline+":complete")
	if err != nil || complete.Descriptor.Digest != captured.Descriptor.Digest {
		t.Fatalf("offline delivery completion: %s %v", complete.Descriptor.Digest, err)
	}
}
