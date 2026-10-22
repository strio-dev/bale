package transport

import (
	"encoding/json"
	"errors"
	specs "github.com/opencontainers/image-spec/specs-go"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/deliveryfile"
	"oras.land/oras-go/v2/content/memory"
)

func TestDeliveryArtifactSplitDefaultsAndAliases(t *testing.T) {
	store := memory.New()
	a, b := object(t, store, "a"), object(t, store, "b")
	snapshot := snapshotFor(t, store, []string{"source.example/components", "source.example/other"}, []v1.Descriptor{a, b})
	metadata := snapshot.Metadata
	metadata.Roots[1].Repository = "source.example/components"
	metadata.Roots[1].Reference = "other"
	metadata.Origins[1].Repository = "source.example/components"
	metadata.Recipe.Include[1].Ref = "source.example/components:other"
	metadata.Dependencies[1].Repository = "source.example/components"
	snapshot, err := bundle.BuildSnapshot(ctx, store, metadata)
	if err != nil {
		t.Fatal(err)
	}
	path := layoutFor(t, store, snapshot.Descriptor)
	file := deliveryfile.File{Bundle: snapshot.Descriptor.Digest, Artifacts: []deliveryfile.Artifact{{Ref: "source.example/components@" + a.Digest.String(), Path: "applications/a"}}}
	plan, err := (&Service{}).PlanPush(ctx, path, "dest.example/release:complete", PushOptions{RepositoryMap: map[string]string{"source.example/components": "default"}, Delivery: &file})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		digest    digest.Digest
		path, tag string
	}{{a.Digest, "applications/a", "stable"}, {b.Digest, "default", "other"}} {
		id := ArtifactID{Repository: "source.example/components", Digest: test.digest}
		relocation, ok := plan.Relocations.Lookup(id)
		if !ok || relocation.Destination.Repository != "dest.example/release/"+test.path {
			t.Fatalf("missing exact relocation for %+v: %+v", id, plan.Relocations)
		}
		found := false
		for _, tag := range plan.Tags {
			if tag.Reference == test.tag && tag.Repository == relocation.Destination.Repository && tag.Descriptor.Digest == test.digest {
				found = true
			}
		}
		if !found {
			t.Fatalf("alias did not follow primary placement: %+v", plan.Tags)
		}
	}
	plan.Relocations[0].Destination.Repository = "dest.example/tampered"
	if _, err := (&Service{}).Push(ctx, plan); !errors.Is(err, ErrInvalid) {
		t.Fatalf("changed relocation accepted: %v", err)
	}
}

func TestDeliveryFileValidationAndExplicitMerge(t *testing.T) {
	store := memory.New()
	a, b := object(t, store, "a"), object(t, store, "b")
	snapshot := snapshotFor(t, store, []string{"one.example/api", "two.example/api"}, []v1.Descriptor{a, b})
	path := layoutFor(t, store, snapshot.Descriptor)
	ref := func(repository string, d digest.Digest) string { return repository + "@" + d.String() }
	tests := []struct {
		name     string
		file     deliveryfile.File
		conflict bool
	}{
		{"stale", deliveryfile.File{Bundle: digest.FromString("stale")}, false},
		{"unknown", deliveryfile.File{Bundle: snapshot.Descriptor.Digest, Artifacts: []deliveryfile.Artifact{{Ref: ref("unknown.example/api", a.Digest), Path: "a"}}}, false},
		{"duplicate", deliveryfile.File{Bundle: snapshot.Descriptor.Digest, Artifacts: []deliveryfile.Artifact{{Ref: ref("one.example/api", a.Digest), Path: "a"}, {Ref: ref("one.example/api", a.Digest), Path: "b"}}}, false},
		{"invalid path", deliveryfile.File{Bundle: snapshot.Descriptor.Digest, Artifacts: []deliveryfile.Artifact{{Ref: ref("one.example/api", a.Digest), Path: "../escape"}}}, false},
		{"alias conflict", deliveryfile.File{Bundle: snapshot.Descriptor.Digest, Artifacts: []deliveryfile.Artifact{{Ref: ref("one.example/api", a.Digest), Path: "shared"}, {Ref: ref("two.example/api", b.Digest), Path: "shared"}}}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := (&Service{}).PlanPush(ctx, path, "dest.example/release:complete", PushOptions{Delivery: &test.file})
			if err == nil {
				t.Fatal("accepted invalid file")
			}
			if test.conflict && !errors.Is(err, ErrConflict) {
				t.Fatalf("wanted conflict: %v", err)
			}
		})
	}
	// Identical content from distinct origins retains both identities and may
	// intentionally share a destination when its captured aliases agree.
	same := snapshotFor(t, store, []string{"one.example/api", "two.example/api"}, []v1.Descriptor{a, a})
	samePath := layoutFor(t, store, same.Descriptor)
	file := deliveryfile.File{Bundle: same.Descriptor.Digest, Artifacts: []deliveryfile.Artifact{{Ref: ref("one.example/api", a.Digest), Path: "shared"}, {Ref: ref("two.example/api", a.Digest), Path: "shared"}}}
	plan, err := (&Service{}).PlanPush(ctx, samePath, "dest.example/release:complete", PushOptions{Delivery: &file})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Relocations) != 2 || len(plan.Repositories) != 1 || len(plan.Tags) != 1 {
		t.Fatalf("merged content lost source identities: %+v", plan)
	}
	for _, relocation := range plan.Relocations {
		if !strings.HasSuffix(relocation.Destination.Repository, "/shared") {
			t.Fatal(relocation)
		}
	}
}

func TestDeliveryPlacementMultiarchAndAttachmentClosure(t *testing.T) {
	store := memory.New()
	subject, attachment := subjectObject(t, store)
	raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{subject}})
	if err != nil {
		t.Fatal(err)
	}
	index := put(t, store, v1.MediaTypeImageIndex, raw)
	repo := "source.example/images/app"
	snapshot := snapshotFor(t, store, []string{repo}, []v1.Descriptor{index})
	m := snapshot.Metadata
	m.Origins = append(m.Origins, bundle.Origin{Repository: repo, Descriptor: subject}, bundle.Origin{Repository: repo, Descriptor: attachment})
	m.Referrers = append(m.Referrers, bundle.Association{Repository: repo, Subject: subject, Descriptor: attachment})
	m.Recipe.Include[0].Referrers = policy("all")
	snapshot, err = bundle.BuildSnapshot(ctx, store, m)
	if err != nil {
		t.Fatal(err)
	}
	path := layoutFor(t, store, snapshot.Descriptor)
	file := deliveryfile.File{Bundle: snapshot.Descriptor.Digest, Artifacts: []deliveryfile.Artifact{{Ref: repo + "@" + subject.Digest.String(), Path: "platform"}, {Ref: repo + "@" + attachment.Digest.String(), Path: "attachments"}}}
	plan, err := (&Service{}).PlanPush(ctx, path, "dest.example/release:complete", PushOptions{Delivery: &file})
	if err != nil {
		t.Fatal(err)
	}
	// The platform manifest remains beside the unchanged index, while the
	// attachment is discoverable beside every physical copy of its subject.
	for _, suffix := range []string{"/images/app", "/platform", "/attachments", ""} {
		expected := "dest.example/release" + suffix
		found := false
		for _, a := range plan.Referrers {
			if a.Repository == expected && a.Subject.Digest == subject.Digest && a.Descriptor.Digest == attachment.Digest {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing attachment beside structural/canonical subject %s: %+v", expected, plan.Referrers)
		}
	}
	if len(plan.Relocations) != 3 {
		t.Fatalf("incidental copies polluted canonical identities: %+v", plan.Relocations)
	}
	relocation, ok := plan.Relocations.Lookup(ArtifactID{Repository: repo, Digest: subject.Digest})
	if !ok || relocation.Destination.Repository != "dest.example/release/platform" {
		t.Fatal(relocation)
	}
}

func TestDeliveryNestedOriginPlacement(t *testing.T) {
	store := memory.New()
	image := object(t, store, "nested image")
	child := snapshotFor(t, store, []string{"source.example/images/api"}, []v1.Descriptor{image})
	outer := snapshotFor(t, store, []string{"source.example/bundles/child"}, []v1.Descriptor{child.Descriptor})
	path := layoutFor(t, store, outer.Descriptor)
	file := deliveryfile.File{Bundle: outer.Descriptor.Digest, Artifacts: []deliveryfile.Artifact{{Ref: "source.example/images/api@" + image.Digest.String(), Path: "applications/api"}}}
	plan, err := (&Service{}).PlanPush(ctx, path, "dest.example/release:complete", PushOptions{Delivery: &file})
	if err != nil {
		t.Fatal(err)
	}
	relocation, ok := plan.Relocations.Lookup(ArtifactID{Repository: "source.example/images/api", Digest: image.Digest})
	if !ok || relocation.Destination.Repository != "dest.example/release/applications/api" {
		t.Fatalf("nested origin lost: %+v", plan)
	}
}

func TestDeliveryAttachmentSelectionRemainsSourceScoped(t *testing.T) {
	store := memory.New()
	subject, a := subjectObject(t, store)
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: put(t, store, v1.MediaTypeEmptyJSON, []byte("{}")), Layers: []v1.Descriptor{}, Subject: &subject, Annotations: map[string]string{"attachment": "b"}})
	if err != nil {
		t.Fatal(err)
	}
	b := put(t, store, v1.MediaTypeImageManifest, raw)
	repositories := []string{"source.example/a", "source.example/b"}
	snapshot := snapshotFor(t, store, repositories, []v1.Descriptor{subject, subject})
	m := snapshot.Metadata
	for i, attachment := range []v1.Descriptor{a, b} {
		m.Recipe.Include[i].Referrers = policy("all")
		m.Origins = append(m.Origins, bundle.Origin{Repository: repositories[i], Descriptor: attachment})
		m.Referrers = append(m.Referrers, bundle.Association{Repository: repositories[i], Subject: subject, Descriptor: attachment})
	}
	snapshot, err = bundle.BuildSnapshot(ctx, store, m)
	if err != nil {
		t.Fatal(err)
	}
	path := layoutFor(t, store, snapshot.Descriptor)
	plan, err := (&Service{}).PlanPush(ctx, path, "dest.example/release:complete", PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, association := range plan.Referrers {
		if association.Repository == "dest.example/release/a" && association.Descriptor.Digest != a.Digest {
			t.Fatalf("source b attachment leaked into a: %+v", association)
		}
		if association.Repository == "dest.example/release/b" && association.Descriptor.Digest != b.Digest {
			t.Fatalf("source a attachment leaked into b: %+v", association)
		}
	}
}
