package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
	"github.com/strio-dev/bale/internal/destination"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

// This backend's repository operations access manifests only. Blobs() exposes a
// physically separate namespace; the backend does not index subjects on push.
type opaqueRegistryCAS struct {
	mu      sync.Mutex
	objects map[digest.Digest][]byte
	descs   map[string]v1.Descriptor
	fetches map[digest.Digest]int
}

func newOpaqueRegistryCAS() *opaqueRegistryCAS {
	return &opaqueRegistryCAS{objects: map[digest.Digest][]byte{}, descs: map[string]v1.Descriptor{}, fetches: map[digest.Digest]int{}}
}

func (s *opaqueRegistryCAS) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.objects[d.Digest]
	if !ok {
		return nil, fmt.Errorf("opaque object %s: %w", d.Digest, errdef.ErrNotFound)
	}
	s.fetches[d.Digest]++
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (s *opaqueRegistryCAS) Exists(ctx context.Context, d v1.Descriptor) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[d.Digest]
	return ok, nil
}

func (s *opaqueRegistryCAS) Push(ctx context.Context, d v1.Descriptor, reader io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := content.ReadAll(reader, d)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[d.Digest]; ok {
		return errdef.ErrAlreadyExists
	}
	s.objects[d.Digest], s.descs[d.Digest.String()] = raw, cloneDescriptor(d)
	return nil
}

func (s *opaqueRegistryCAS) Resolve(ctx context.Context, ref string) (v1.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.descs[ref]
	if !ok {
		return v1.Descriptor{}, fmt.Errorf("opaque reference %s: %w", ref, errdef.ErrNotFound)
	}
	return cloneDescriptor(d), nil
}

type opaqueRegistry struct {
	*opaqueRegistryCAS
	blobs     *opaqueRegistryCAS
	native    func(context.Context, v1.Descriptor, func(ReferrerPage) error) error
	failedTag error
	tagWrites int
}

func newOpaqueRegistry() *opaqueRegistry {
	return &opaqueRegistry{opaqueRegistryCAS: newOpaqueRegistryCAS(), blobs: newOpaqueRegistryCAS()}
}

func (r *opaqueRegistry) Blobs() RegistryBlobStore { return r.blobs }

func (r *opaqueRegistry) Push(ctx context.Context, d v1.Descriptor, reader io.Reader) error {
	if !bundle.IsEnvelope(d.MediaType) {
		return fmt.Errorf("repository accepts manifests only: %w", errdef.ErrUnsupported)
	}
	raw, err := content.ReadAll(reader, d)
	if err != nil {
		return err
	}
	if d.MediaType == v1.MediaTypeImageManifest {
		var manifest v1.Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return err
		}
		for _, layer := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
			if ok, err := r.blobs.Exists(ctx, layer); err != nil || !ok {
				return fmt.Errorf("manifest blob %s unavailable: %w", layer.Digest, errors.Join(errdef.ErrNotFound, err))
			}
		}
	}
	return r.opaqueRegistryCAS.Push(ctx, d, bytes.NewReader(raw))
}

func (r *opaqueRegistry) Tag(ctx context.Context, d v1.Descriptor, ref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ok, err := r.Exists(ctx, d); err != nil || !ok {
		return errors.Join(errdef.ErrNotFound, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tagWrites++
	if r.failedTag != nil {
		err := r.failedTag
		r.failedTag = nil
		return err
	}
	r.descs[ref] = cloneDescriptor(d)
	return nil
}

func (r *opaqueRegistry) ReferrerPages(ctx context.Context, subject v1.Descriptor, fn func(ReferrerPage) error) error {
	if r.native == nil {
		return errdef.ErrUnsupported
	}
	return r.native(ctx, subject, fn)
}

func opaqueService(t *testing.T, repositories map[string]RegistryRepository) *Service {
	t.Helper()
	s, err := NewService(Options{RepositoryFactory: func(ctx context.Context, name string) (RegistryRepository, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r, ok := repositories[name]
		if !ok {
			return nil, fmt.Errorf("unexpected registry access: %s", name)
		}
		return r, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func opaqueObject(t *testing.T, r *opaqueRegistry, payload string) v1.Descriptor {
	t.Helper()
	config := put(t, r.blobs, v1.MediaTypeEmptyJSON, []byte("{}"))
	layer := put(t, r.blobs, "application/example", []byte(payload))
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	return put(t, r, v1.MediaTypeImageManifest, raw)
}

func opaqueIndex(t *testing.T, r *opaqueRegistry, subject *v1.Descriptor, children ...v1.Descriptor) v1.Descriptor {
	t.Helper()
	if children == nil {
		children = []v1.Descriptor{}
	}
	raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Subject: subject, Manifests: children})
	if err != nil {
		t.Fatal(err)
	}
	return put(t, r, v1.MediaTypeImageIndex, raw)
}

func opaqueDeclaration(t *testing.T, repo string, subject v1.Descriptor) AttachRequest {
	t.Helper()
	request, err := PrepareDependencyDeclaration(t.Context(), Root{Repository: repo, Descriptor: subject}, dependency.Declaration{SchemaVersion: 1, Dependencies: []dependency.Reference{{Ref: "dependencies.invalid/base:stable"}}})
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func fallbackInventory(t *testing.T, r *opaqueRegistry, subject v1.Descriptor) (v1.Descriptor, v1.Index) {
	t.Helper()
	d, err := r.Resolve(t.Context(), strings.Replace(subject.Digest.String(), ":", "-", 1))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := content.FetchAll(t.Context(), r, d)
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	return d, index
}

func TestRegistryBackendAttachPinsSubjectAndPreservesFallback(t *testing.T) {
	const name = "opaque.invalid/components/tool"
	r := newOpaqueRegistry()
	subject := opaqueObject(t, r, "original")
	if err := r.Tag(t.Context(), subject, "stable"); err != nil {
		t.Fatal(err)
	}
	s := opaqueService(t, map[string]RegistryRepository{name: r})
	root, err := s.Resolve(t.Context(), name+":stable")
	if err != nil || !content.Equal(root.Descriptor, subject) {
		t.Fatalf("resolved subject=%+v err=%v", root.Descriptor, err)
	}
	request := opaqueDeclaration(t, name, root.Descriptor)
	other := opaqueObject(t, r, "changed tag")
	if err := r.Tag(t.Context(), other, "stable"); err != nil {
		t.Fatal(err)
	}
	old := opaqueIndex(t, r, &subject)
	old.Annotations = map[string]string{"existing": "preserve"}
	oldInventory := opaqueIndex(t, r, nil, old)
	if err := r.Tag(t.Context(), oldInventory, strings.Replace(subject.Digest.String(), ":", "-", 1)); err != nil {
		t.Fatal(err)
	}
	result, err := s.Attach(t.Context(), request)
	if err != nil || !result.Published || !result.Discoverable || !content.Equal(result.Subject, subject) {
		t.Fatalf("attachment=%+v err=%v", result, err)
	}
	_, inventory := fallbackInventory(t, r, subject)
	if len(inventory.Manifests) != 2 {
		t.Fatalf("existing referrer lost or duplicated: %+v", inventory.Manifests)
	}
	var preserved bool
	for _, d := range inventory.Manifests {
		if d.Digest == old.Digest {
			preserved = d.Annotations["existing"] == "preserve"
		}
	}
	if !preserved {
		t.Fatal("existing inventory descriptor metadata was discarded")
	}
	if ok, _ := r.Exists(t.Context(), oldInventory); !ok {
		t.Fatal("previous inventory manifest was deleted")
	}
	raw, err := content.FetchAll(t.Context(), r, request.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	payload, err := content.FetchAll(t.Context(), r.blobs, manifest.Layers[0])
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := dependency.Decode(payload)
	if err != nil || len(declaration.Dependencies) != 1 || declaration.Dependencies[0].Ref != "dependencies.invalid/base:stable" {
		t.Fatalf("dependency selector changed: %+v err=%v", declaration, err)
	}
	refs, err := s.ListReferrers(t.Context(), request.Subject, ListReferrersOptions{})
	if err != nil || len(refs) != 2 {
		t.Fatalf("discovery=%+v err=%v", refs, err)
	}
}

func TestRegistryBackendResolvedStoreReadsPayloadsFromBlobNamespace(t *testing.T) {
	const name = "opaque.invalid/components/tool"
	r := newOpaqueRegistry()
	subject := opaqueObject(t, r, "payload")
	blob := content.NewDescriptorFromBytes("application/example", []byte("payload"))
	if exists, err := r.Exists(t.Context(), blob); err != nil || exists {
		t.Fatalf("payload unexpectedly available in manifest namespace: exists=%v err=%v", exists, err)
	}
	s := opaqueService(t, map[string]RegistryRepository{name: r})
	root, err := s.Resolve(t.Context(), name+"@"+subject.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	exists, err := root.Store.Exists(t.Context(), blob)
	if err != nil || !exists {
		t.Fatalf("resolved store cannot find payload blob: exists=%v err=%v", exists, err)
	}
	raw, err := content.FetchAll(t.Context(), root.Store, blob)
	if err != nil || string(raw) != "payload" {
		t.Fatalf("resolved store cannot read payload blob: raw=%q err=%v", raw, err)
	}
}

func TestRegistryBackendInspectsDependencyPayloadsFromBlobNamespace(t *testing.T) {
	const name = "opaque.invalid/components/tool"
	r := newOpaqueRegistry()
	subject := opaqueObject(t, r, "subject")
	s := opaqueService(t, map[string]RegistryRepository{name: r})
	request := opaqueDeclaration(t, name, subject)
	if _, err := s.Attach(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	declarations, err := s.InspectDependencies(t.Context(), request.Subject)
	if err != nil || len(declarations) != 1 {
		t.Fatalf("inspect dependency attachment payload: declarations=%+v err=%v", declarations, err)
	}
	declaration := declarations[0]
	if !content.Equal(declaration.Descriptor, request.Descriptor) || !content.Equal(declaration.Subject, subject) || len(declaration.Declaration.Dependencies) != 1 || declaration.Declaration.Dependencies[0].Ref != "dependencies.invalid/base:stable" {
		t.Fatalf("dependency inspection changed the captured declaration: %+v", declaration)
	}
}

func TestRegistryBackendInspectsDestinationPayloadsFromBlobNamespace(t *testing.T) {
	const name = "opaque.invalid/delivered/tool"
	r := newOpaqueRegistry()
	subject := opaqueObject(t, r, "subject")
	record := destination.Record{
		SchemaVersion: destination.Version,
		Bundle:        "unavailable.invalid/bundles@" + digest.FromString("bundle").String(),
		Source:        "unavailable.invalid/components/tool@" + subject.Digest.String(),
		Destination:   name + "@" + subject.Digest.String(),
		Resolutions:   []destination.Resolution{},
	}
	descriptor, store, err := destination.Prepare(t.Context(), subject, record)
	if err != nil {
		t.Fatal(err)
	}
	s := opaqueService(t, map[string]RegistryRepository{name: r})
	if _, err := s.Attach(t.Context(), AttachRequest{Subject: record.Destination, Descriptor: descriptor, Store: store}); err != nil {
		t.Fatal(err)
	}
	records, err := s.InspectDestinations(t.Context(), record.Destination)
	if err != nil || len(records) != 1 {
		t.Fatalf("inspect destination attachment payload: records=%+v err=%v", records, err)
	}
	if !records[0].Local || !content.Equal(records[0].Descriptor, descriptor) || !content.Equal(records[0].Subject, subject) || records[0].Record.Source != record.Source {
		t.Fatalf("destination inspection changed the recorded placement: %+v", records[0])
	}
}

func TestRegistryBackendInterruptedInventoryRetryRepairsExistingManifest(t *testing.T) {
	const name = "opaque.invalid/components/tool"
	r := newOpaqueRegistry()
	subject := opaqueObject(t, r, "subject")
	request := opaqueDeclaration(t, name, subject)
	s := opaqueService(t, map[string]RegistryRepository{name: r})
	interrupted := errors.New("caller inventory tag interrupted")
	r.failedTag = interrupted
	result, err := s.Attach(t.Context(), request)
	if !errors.Is(err, interrupted) || !result.Published || result.Discoverable {
		t.Fatalf("partial publication=%+v err=%v", result, err)
	}
	if ok, _ := r.Exists(t.Context(), request.Descriptor); !ok {
		t.Fatal("test did not reach accepted manifest publication")
	}
	result, err = s.Attach(t.Context(), request)
	if err != nil || !result.Published || !result.Discoverable {
		t.Fatalf("deterministic retry=%+v err=%v", result, err)
	}
	first, index := fallbackInventory(t, r, subject)
	if len(index.Manifests) != 1 || index.Manifests[0].Digest != request.Descriptor.Digest {
		t.Fatalf("retry inventory=%+v", index)
	}
	if _, err := s.Attach(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	second, _ := fallbackInventory(t, r, subject)
	if first.Digest != second.Digest {
		t.Fatal("healthy retry changed deterministic inventory")
	}
}

func TestRegistryBackendNativeFailureDoesNotUseFallback(t *testing.T) {
	const name = "opaque.invalid/components/tool"
	r := newOpaqueRegistry()
	subject := opaqueObject(t, r, "subject")
	request := opaqueDeclaration(t, name, subject)
	denied := errors.New("caller native registry authorization denied")
	r.native = func(context.Context, v1.Descriptor, func(ReferrerPage) error) error { return denied }
	s := opaqueService(t, map[string]RegistryRepository{name: r})
	result, err := s.Attach(t.Context(), request)
	if !errors.Is(err, denied) || result.Discoverable {
		t.Fatalf("native authorization failure=%+v err=%v", result, err)
	}
	if r.tagWrites != 0 {
		t.Fatal("native authorization failure was hidden by writing fallback inventory")
	}
}

func TestRegistryBackendValidatesNativeInventoryBeforeReferrerFetch(t *testing.T) {
	for _, kind := range []string{"valid unknown metadata", "media type", "length", "digest", "subject", "count budget", "wrong attachment subject"} {
		t.Run(kind, func(t *testing.T) {
			const name = "opaque.invalid/components/tool"
			r := newOpaqueRegistry()
			subject := opaqueObject(t, r, "subject")
			attachmentSubject := subject
			if kind == "wrong attachment subject" {
				attachmentSubject = opaqueObject(t, r, "other")
			}
			attachment := opaqueIndex(t, r, &attachmentSubject)
			index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{attachment}}
			if kind == "subject" {
				index.Subject = &subject
			}
			if kind == "count budget" {
				index.Manifests = append(index.Manifests, attachment)
			}
			raw, err := json.Marshal(index)
			if err != nil {
				t.Fatal(err)
			}
			r.native = func(_ context.Context, _ v1.Descriptor, fn func(ReferrerPage) error) error {
				page := ReferrerPage{MediaType: v1.MediaTypeImageIndex, Size: int64(len(raw)), Digest: digest.FromBytes(raw), Content: bytes.NewReader(raw)}
				switch kind {
				case "valid unknown metadata":
					page.Digest, page.Size = "", -1
				case "media type":
					page.MediaType = "application/json"
				case "length":
					page.Size++
				case "digest":
					page.Digest = digest.FromString("different inventory")
				}
				return fn(page)
			}
			s := opaqueService(t, map[string]RegistryRepository{name: r})
			ctx := t.Context()
			want := ErrIntegrity
			if kind == "count budget" {
				ctx = context.WithValue(ctx, destinationInspectionBudgetKey{}, &destinationInspectionBudget{remaining: 4 << 20, maxReferrers: 1})
				want = ErrUnsupported
			}
			refs, err := s.ListReferrers(ctx, name+"@"+subject.Digest.String(), ListReferrersOptions{})
			if kind == "valid unknown metadata" {
				if err != nil || len(refs) != 1 || !content.Equal(refs[0], attachment) {
					t.Fatalf("valid native inventory rejected: %+v %v", refs, err)
				}
				return
			}
			if !errors.Is(err, want) {
				t.Fatalf("invalid %s inventory accepted: %v", kind, err)
			}
			if kind != "wrong attachment subject" && r.fetches[attachment.Digest] != 0 {
				t.Fatal("fetched attachment after raw inventory failed validation")
			}
		})
	}
}

func TestRegistryBackendTransfersEnvelopeLayersIntoBothNamespaces(t *testing.T) {
	source, destination := newOpaqueRegistry(), newOpaqueRegistry()
	child := opaqueObject(t, source, "child")
	childRaw, err := content.FetchAll(t.Context(), source, child)
	if err != nil {
		t.Fatal(err)
	}
	put(t, source.blobs, child.MediaType, childRaw)
	config := put(t, source.blobs, v1.MediaTypeEmptyJSON, []byte("{}"))
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{child}})
	if err != nil {
		t.Fatal(err)
	}
	root := put(t, source, v1.MediaTypeImageManifest, raw)
	const origin = "opaque.invalid/source/component"
	const target = "opaque.invalid/destination"
	s := opaqueService(t, map[string]RegistryRepository{origin: source, target + "/source/component": destination})
	resolved, err := s.Resolve(t.Context(), origin+"@"+root.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	resolved.Referrers = policy("none")
	plan, err := s.Plan(t.Context(), TransferRequest{Roots: []Root{resolved}, Destination: target})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	for _, store := range []content.ReadOnlyStorage{destination, destination.blobs} {
		actual, err := content.FetchAll(t.Context(), store, child)
		if err != nil || !bytes.Equal(actual, childRaw) {
			t.Fatalf("envelope layer absent from destination namespace: %v", err)
		}
	}
	if err := bundle.VerifyClosure(t.Context(), resolved.Store, resolved.Descriptor); err != nil {
		t.Fatal(err)
	}
}

type tagListingOpaqueRegistry struct{ *opaqueRegistry }

func (r tagListingOpaqueRegistry) Tags(ctx context.Context, last string, fn func([]string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	tags := make([]string, 0, len(r.descs))
	for tag := range r.descs {
		if !strings.Contains(tag, ":") && tag > last {
			tags = append(tags, tag)
		}
	}
	r.mu.Unlock()
	sort.Strings(tags)
	return fn(tags)
}

func TestRegistryBackendAllTagsNeedsExplicitCapability(t *testing.T) {
	const name = "opaque.invalid/components/tool"
	r := newOpaqueRegistry()
	subject := opaqueObject(t, r, "subject")
	if err := r.Tag(t.Context(), subject, "stable"); err != nil {
		t.Fatal(err)
	}
	tags := []string{}
	recipe := config.Recipe{Include: []config.Selection{{Repository: name, Tags: &tags, Referrers: policy("none"), Dependencies: dependency.Policy{Follow: "none"}}}}
	s := opaqueService(t, map[string]RegistryRepository{name: r})
	if _, _, err := s.capture(t.Context(), recipe); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("all-tags silently accepted backend without listing: %v", err)
	}
	s = opaqueService(t, map[string]RegistryRepository{name: tagListingOpaqueRegistry{r}})
	_, snapshot, err := s.capture(t.Context(), recipe)
	if err != nil || len(snapshot.Metadata.Roots) != 1 || snapshot.Metadata.Roots[0].Reference != "stable" {
		t.Fatalf("caller tag enumeration: %+v err=%v", snapshot.Metadata.Roots, err)
	}
}

func TestRegistryBackendNativePublicationDoesNotClaimMissingReferrer(t *testing.T) {
	const name = "opaque.invalid/components/tool"
	r := newOpaqueRegistry()
	subject := opaqueObject(t, r, "subject")
	request := opaqueDeclaration(t, name, subject)
	raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{}})
	if err != nil {
		t.Fatal(err)
	}
	r.native = func(_ context.Context, _ v1.Descriptor, fn func(ReferrerPage) error) error {
		return fn(ReferrerPage{MediaType: v1.MediaTypeImageIndex, Size: int64(len(raw)), Content: bytes.NewReader(raw)})
	}
	s := opaqueService(t, map[string]RegistryRepository{name: r})
	result, err := s.Attach(t.Context(), request)
	if !errors.Is(err, ErrIntegrity) || !result.Published || result.Discoverable {
		t.Fatalf("missing native referrer reported as discoverable: %+v %v", result, err)
	}
	if r.tagWrites != 0 {
		t.Fatal("successful native API was bypassed by publishing a fallback inventory")
	}
}

func TestRegistryBackendFactoryErrorRetainsIdentity(t *testing.T) {
	for _, cause := range []error{errors.New("caller registry policy denied access"), context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			calls := 0
			s, err := NewService(Options{RepositoryFactory: func(context.Context, string) (RegistryRepository, error) {
				calls++
				return nil, cause
			}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Resolve(t.Context(), "opaque.invalid/component:stable"); !errors.Is(err, cause) || errors.Is(err, ErrInvalid) {
				t.Fatalf("caller backend error lost its transport classification: %v", err)
			}
			if _, err := s.Resolve(t.Context(), "opaque.invalid/component"); !errors.Is(err, ErrInvalid) || calls != 1 {
				t.Fatalf("invalid reference reached caller backend: calls=%d err=%v", calls, err)
			}
		})
	}
}
