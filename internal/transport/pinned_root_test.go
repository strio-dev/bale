package transport

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/config"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func TestPinnedRootArchiveMatchesExternalLayout(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	root := Root{Descriptor: snapshotFor(t, store, []string{"example.test/source"}, []v1.Descriptor{object(t, store, "payload")}).Descriptor, Store: store}
	layout := layoutFor(t, store, root.Descriptor)

	var external, pinned bytes.Buffer
	s := &Service{}
	if _, err := s.SaveTo(ctx, layout, &external); err != nil {
		t.Fatal(err)
	}
	saved, err := s.SaveRootTo(ctx, root, &pinned)
	if err != nil || !content.Equal(saved.Descriptor, root.Descriptor) || saved.LayoutPath != "" {
		t.Fatalf("save root stream: %+v, %v", saved, err)
	}
	path := filepath.Join(t.TempDir(), "bundle.tar")
	saved, err = s.SaveRoot(ctx, root, path)
	if err != nil || saved.ArchivePath != path || !content.Equal(saved.Descriptor, root.Descriptor) {
		t.Fatalf("save root file: %+v, %v", saved, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, pinned.Bytes()) || !bytes.Equal(raw, external.Bytes()) {
		t.Fatal("pinned root did not produce the external layout's deterministic archive")
	}
}

func TestPinnedMetadataCapturePlansOfflineAndPublishesPinnedPayload(t *testing.T) {
	ctx := t.Context()
	const (
		sourceName = "source.invalid/components/tool"
		targetName = "destination.invalid/releases/tool"
		alias      = "managed:v1"
		tag        = "bundle"
	)
	source, target, member := newOpaqueRegistry(), newOpaqueRegistry(), newOpaqueRegistry()
	subject := opaqueObject(t, source, "original payload")
	if err := source.Tag(ctx, subject, "stable"); err != nil {
		t.Fatal(err)
	}
	s := opaqueService(t, map[string]RegistryRepository{
		sourceName: source, targetName: target, targetName + "/components/tool": member,
	})
	store := memory.New()
	created, capture, err := s.CreateInto(ctx, config.Recipe{Include: []config.Selection{dependencySelection(sourceName + ":stable")}}, store, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	root := Root{Descriptor: created.Descriptor, Store: store}
	offline := opaqueService(t, nil)
	inspected, err := offline.InspectMetadataRoot(ctx, root)
	if err != nil || !inspected.MetadataOnly || !content.Equal(inspected.Descriptor, root.Descriptor) {
		t.Fatalf("inspect pinned metadata: %+v, %v", inspected, err)
	}
	exported, err := offline.ExportDeliveryMetadataRoot(ctx, root)
	if err != nil || exported.Bundle != root.Descriptor.Digest || len(exported.Artifacts) != 1 {
		t.Fatalf("export pinned metadata: %+v, %v", exported, err)
	}
	plan, err := offline.PlanPushRoot(ctx, root, capture, alias, targetName+":"+tag, PushOptions{Delivery: &exported})
	if err != nil || !plan.MetadataOnly || plan.LayoutPath != alias || !content.Equal(plan.Descriptor, root.Descriptor) {
		t.Fatalf("plan pinned metadata: %+v, %v", plan, err)
	}
	if err := source.Tag(ctx, opaqueObject(t, source, "replacement tag payload"), "stable"); err != nil {
		t.Fatal(err)
	}
	pushed, err := s.PushRoot(ctx, root, capture, plan)
	if err != nil || !content.Equal(pushed.Descriptor, root.Descriptor) || pushed.Stats.BlobBytesDownloaded == 0 {
		t.Fatalf("push pinned metadata: %+v, %v", pushed, err)
	}
	published, err := target.Resolve(ctx, tag)
	if err != nil || !content.Equal(published, root.Descriptor) {
		t.Fatalf("completion alias: %+v, %v", published, err)
	}
	published, err = member.Resolve(ctx, "stable")
	if err != nil || !content.Equal(published, subject) {
		t.Fatalf("member alias did not use the pinned subject: %+v, %v", published, err)
	}
}
