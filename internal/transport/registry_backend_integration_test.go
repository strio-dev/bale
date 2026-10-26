package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
)

func TestRegistryBackendCaptureRefreshAndDestinationOnlyDelivery(t *testing.T) {
	const appName = "source.invalid/components/app"
	const baseName = "source.invalid/components/base"
	const commonName = "source.invalid/components/common"
	const holdingName = "holding.invalid/bundles"
	const destination = "mirror.invalid/release"
	appRegistry, baseRegistry, commonRegistry := newOpaqueRegistry(), newOpaqueRegistry(), newOpaqueRegistry()
	repositories := map[string]RegistryRepository{
		appName: appRegistry, baseName: baseRegistry, commonName: commonRegistry,
		holdingName: newOpaqueRegistry(), destination: newOpaqueRegistry(),
		destination + "/components/app": newOpaqueRegistry(), destination + "/components/base": newOpaqueRegistry(), destination + "/components/common": newOpaqueRegistry(),
	}
	s := opaqueService(t, repositories)
	app := opaqueObject(t, appRegistry, "app")
	base := opaqueObject(t, baseRegistry, "base version one")
	common := opaqueObject(t, commonRegistry, "shared component")
	for _, entry := range []struct {
		r *opaqueRegistry
		d v1.Descriptor
	}{{appRegistry, app}, {baseRegistry, base}, {commonRegistry, common}} {
		if err := entry.r.Tag(t.Context(), entry.d, "stable"); err != nil {
			t.Fatal(err)
		}
	}
	attach := func(name string, subject v1.Descriptor, refs ...dependency.Reference) v1.Descriptor {
		t.Helper()
		request, err := PrepareDependencyDeclaration(t.Context(), Root{Repository: name, Descriptor: subject}, dependency.Declaration{Dependencies: refs})
		if err != nil {
			t.Fatal(err)
		}
		result, err := s.Attach(t.Context(), request)
		if err != nil || !result.Discoverable {
			t.Fatalf("declare component dependencies: %+v %v", result, err)
		}
		return request.Descriptor
	}
	appDeclaration := attach(appName, app, dependency.Reference{Ref: baseName + ":stable"}, dependency.Reference{Ref: commonName + ":stable"}, dependency.Reference{Ref: commonName + ":stable"})
	attach(baseName, base, dependency.Reference{Ref: commonName + ":stable"})
	created, err := s.Create(t.Context(), config.Recipe{Include: []config.Selection{dependencySelection(appName + ":stable")}}, holdingName+":bundle", CreateOptions{})
	if err != nil || !created.Published {
		t.Fatalf("capture parent and recursive dependencies: %+v %v", created, err)
	}
	commonOrigins := 0
	for _, origin := range created.Metadata.Origins {
		if origin.Repository == commonName && origin.Descriptor.Digest == common.Digest {
			commonOrigins++
		}
	}
	if commonOrigins != 1 || !capturedOrigin(created.Metadata, base) {
		t.Fatalf("recursive capture failed to deduplicate shared dependency: %+v", created.Metadata.Origins)
	}
	newBase := opaqueObject(t, baseRegistry, "base version two")
	attach(baseName, newBase, dependency.Reference{Ref: commonName + ":stable"})
	if err := baseRegistry.Tag(t.Context(), newBase, "stable"); err != nil {
		t.Fatal(err)
	}
	paths := map[PullMode]string{}
	for _, mode := range []PullMode{PullLocked, PullLatest} {
		path := filepath.Join(t.TempDir(), string(mode))
		pulled, err := s.Pull(t.Context(), holdingName+":bundle", path, mode)
		if err != nil {
			t.Fatal(err)
		}
		store, err := oci.NewStorage(path)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := bundle.LoadSnapshotGraph(t.Context(), store, pulled.Descriptor)
		if err != nil {
			t.Fatal(err)
		}
		if mode == PullLocked && (pulled.Descriptor.Digest != created.Descriptor.Digest || !capturedOrigin(snapshot.Metadata, base) || capturedOrigin(snapshot.Metadata, newBase)) {
			t.Fatal("locked pull refreshed a dependency selector")
		}
		if mode == PullLatest && (!capturedOrigin(snapshot.Metadata, newBase) || capturedOrigin(snapshot.Metadata, base)) {
			t.Fatal("latest pull retained the old dependency digest")
		}
		paths[mode] = path
	}
	plan, err := s.PlanPush(t.Context(), paths[PullLatest], destination+":bundle", PushOptions{RepositoryMap: map[string]string{appName: "components/app", baseName: "components/base", commonName: "components/common"}})
	if err != nil {
		t.Fatal(err)
	}
	// Any subsequent source access fails in the factory. Delivery and inspection
	// must use the captured closure and destination repositories only.
	for _, name := range []string{appName, baseName, commonName, holdingName} {
		delete(repositories, name)
	}
	pushed, err := s.Push(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		source string
		d      v1.Descriptor
	}{{appName, app}, {baseName, newBase}, {commonName, common}} {
		relocation, ok := pushed.Relocations.Lookup(ArtifactID{Repository: entry.source, Digest: entry.d.Digest})
		if !ok || relocation.Destination.Digest != entry.d.Digest {
			t.Fatalf("missing byte-preserving relocation for %s", entry.source)
		}
		resolved, err := s.Resolve(t.Context(), relocation.Destination.Repository+"@"+entry.d.Digest.String())
		if err != nil || !content.Equal(resolved.Descriptor, entry.d) {
			t.Fatalf("destination subject changed: %+v %v", resolved.Descriptor, err)
		}
	}
	appReference := destination + "/components/app@" + app.Digest.String()
	refs, err := s.ListReferrers(t.Context(), appReference, ListReferrersOptions{})
	if err != nil || !hasAttachment(refs, appDeclaration) {
		t.Fatalf("delivered dependency attachment missing: %+v %v", refs, err)
	}
	records, err := s.InspectDestinations(t.Context(), appReference)
	if err != nil || len(records) != 1 || !records[0].Local || records[0].Record.Source != appName+"@"+app.Digest.String() {
		t.Fatalf("destination mapping missing: %+v %v", records, err)
	}
}

func TestRegistryBackendOversizedMergePreservesExistingInventory(t *testing.T) {
	const name = "opaque.invalid/components/tool"
	r := newOpaqueRegistry()
	subject := opaqueObject(t, r, "subject")
	oldAttachment := opaqueIndex(t, r, &subject)
	oldInventory := opaqueIndex(t, r, nil, oldAttachment)
	if err := r.Tag(t.Context(), oldInventory, strings.Replace(subject.Digest.String(), ":", "-", 1)); err != nil {
		t.Fatal(err)
	}
	request := opaqueDeclaration(t, name, subject)
	raw, err := content.FetchAll(t.Context(), request.Store, request.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, d := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
		payload, err := content.FetchAll(t.Context(), request.Store, d)
		if err != nil {
			t.Fatal(err)
		}
		put(t, r.blobs, d.MediaType, payload)
	}
	s := opaqueService(t, map[string]RegistryRepository{name: r})
	repo, err := s.repository(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	repo.MaxMetadataBytes = oldInventory.Size + 1
	beforeObjects, beforeTags := len(r.objects), r.tagWrites
	if err := repo.Push(t.Context(), request.Descriptor, bytes.NewReader(raw)); err == nil {
		t.Fatal("inventory merge exceeded metadata limit")
	}
	actual, _ := fallbackInventory(t, r, subject)
	if actual.Digest != oldInventory.Digest || r.tagWrites != beforeTags || len(r.objects) != beforeObjects+1 {
		t.Fatal("oversized merge published a replacement inventory or changed its tag")
	}
}

type registryCountingReader struct {
	io.Reader
	reads int
}

func (r *registryCountingReader) Read(p []byte) (int, error) {
	r.reads++
	return r.Reader.Read(p)
}

func TestRegistryBackendCannotSwallowInventoryValidationFailure(t *testing.T) {
	const name = "opaque.invalid/components/tool"
	r := newOpaqueRegistry()
	subject := opaqueObject(t, r, "subject")
	second := &registryCountingReader{Reader: strings.NewReader(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)}
	r.native = func(_ context.Context, _ v1.Descriptor, fn func(ReferrerPage) error) error {
		_ = fn(ReferrerPage{MediaType: "application/json", Size: -1, Content: strings.NewReader("{}")})
		_ = fn(ReferrerPage{MediaType: v1.MediaTypeImageIndex, Size: -1, Content: second})
		return nil
	}
	s := opaqueService(t, map[string]RegistryRepository{name: r})
	_, err := s.ListReferrers(t.Context(), name+"@"+subject.Digest.String(), ListReferrersOptions{})
	if !errors.Is(err, ErrIntegrity) || second.reads != 0 {
		t.Fatalf("callback validation failure swallowed: err=%v later reads=%d", err, second.reads)
	}
}
