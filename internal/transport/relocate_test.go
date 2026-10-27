package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func TestDirectPlanCallerStoreAndImmutableView(t *testing.T) {
	source := memory.New()
	root := object(t, source, "component-package")
	root.Annotations = map[string]string{"owner": "original"}
	// This endpoint deliberately rejects every mutation. An empty destination
	// needs no GETs without aliases or subjects; source provenance is unavailable.
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writes++
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	service, err := NewService(Options{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	request := TransferRequest{Roots: []Root{{Repository: "unavailable.example/components", Descriptor: root, Store: source, Referrers: policy("none")}}, Destination: host + "/release", RepositoryMap: map[string]string{"unavailable.example/components": "application"}}
	plan, err := service.Plan(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("planning wrote destination %d times", writes)
	}
	if len(plan.graph.meta.Roots) != 0 {
		t.Fatal("direct plan generated native bundle roots")
	}
	view := plan.View()
	if len(view.Relocations) != 1 || len(view.Repositories) != 1 {
		t.Fatalf("wrong view: %+v", view)
	}
	id := ArtifactID{Repository: "unavailable.example/components", Digest: root.Digest}
	relocation, ok := view.Relocations.Lookup(id)
	if !ok || relocation.Destination.Repository != host+"/release/application" {
		t.Fatalf("wrong relocation: %+v", relocation)
	}
	request.RepositoryMap[id.Repository] = "changed"
	request.Roots[0].Descriptor.Annotations["owner"] = "changed"
	view.RepositoryMap[id.Repository] = "changed"
	view.Relocations[0].Descriptor.Annotations["owner"] = "changed"
	view.Repositories[0].Roots[0].Annotations["owner"] = "changed"
	observed := plan.View()
	if observed.RepositoryMap[id.Repository] != host+"/release/application" || observed.Relocations[0].Descriptor.Annotations["owner"] != "original" {
		t.Fatalf("mutable plan: %+v", observed)
	}
	if plan.graph.meta.SchemaVersion != 1 {
		t.Fatal("invalid capture graph")
	}
}

func TestDirectPlanRequiresExplicitReferrerCapability(t *testing.T) {
	source := newMetadataStore()
	root := object(t, source, "package")
	service, _ := NewService(Options{})
	request := TransferRequest{Roots: []Root{{Repository: "source.example/components", Descriptor: root, Store: source, Referrers: config.Policy{Include: "all"}}}, Destination: "target.example/release"}
	if _, err := service.Plan(ctx, request); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("wanted unsupported referrer capability, got %v", err)
	}
	request.Roots[0].Referrers = config.Policy{Require: []config.RequiredReferrer{{Digest: root.Digest}}}
	if _, err := service.Plan(ctx, request); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("wanted unsupported required resolution, got %v", err)
	}
}

func TestDirectResolvePinsTagsAndPlansMetadataOnly(t *testing.T) {
	source, name, service := testRegistry(t)
	root := object(t, source.store, "large-payload")
	source.tags["stable"] = root
	resolved, err := service.Resolve(ctx, name+":stable")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Aliases) != 1 || resolved.Aliases[0] != "stable" {
		t.Fatalf("missing source alias: %+v", resolved)
	}
	// No destination tags needed, avoiding cross-registry interaction in this
	// metadata-read accounting test.
	resolved.Aliases = nil
	plan, err := service.Plan(ctx, TransferRequest{Roots: []Root{resolved}, Destination: "target.example/release"})
	if err != nil {
		t.Fatal(err)
	}
	if source.blobReads != 0 {
		t.Fatalf("planned by eagerly reading %d payloads", source.blobReads)
	}
	if plan.View().Relocations[0].Source.Digest != root.Digest {
		t.Fatal("lost pinned digest")
	}
	source.tags["stable"] = object(t, source.store, "moved")
	if source.headTags["stable"] != 1 {
		t.Fatalf("tag resolved %d times", source.headTags["stable"])
	}
}

func TestDirectPlanCancellationAndIntegrityErrors(t *testing.T) {
	service, _ := NewService(Options{})
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := service.Plan(canceled, TransferRequest{Destination: "target.example/release"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	var detail *Error
	if !errors.As(err, &detail) || detail.Op != "plan" {
		t.Fatalf("missing operation details: %v", err)
	}
	source := memory.New()
	root := object(t, source, "package")
	root.Size++
	_, err = service.Plan(ctx, TransferRequest{Roots: []Root{{Repository: "source.example/components", Descriptor: root, Store: source, Referrers: policy("none")}}, Destination: "target.example/release"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("wanted missing descriptor failure: %v", err)
	}
	if _, err := service.Execute(ctx, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wanted invalid plan: %v", err)
	}
}

func TestDirectPlanRequiredReferrerChecksActualSubject(t *testing.T) {
	source, name, service := testRegistry(t)
	subject := object(t, source.store, "subject")
	other := object(t, source.store, "other")
	ref := refIndex(t, source, other, nil)
	remote, err := service.repository(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Plan(ctx, TransferRequest{Roots: []Root{{Repository: name, Descriptor: subject, Store: remote, Referrers: policy("none", ref)}}, Destination: "target.example/release"})
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("wrong subject accepted: %v", err)
	}
}

func TestDirectPlanPreservesDistinctCallerStoresWithSameRepository(t *testing.T) {
	a, b := newMetadataStore(), newMetadataStore()
	first, second := object(t, a, "first-package"), object(t, b, "second-package")
	service, _ := NewService(Options{})
	request := TransferRequest{Roots: []Root{
		{Repository: "source.example/components", Descriptor: first, Store: a, Referrers: policy("none")},
		{Repository: "source.example/components", Descriptor: second, Store: b, Referrers: policy("none")},
	}, Destination: "target.example/release"}
	plan, err := service.Plan(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.View().Relocations) != 2 {
		t.Fatal("lost root")
	}
	for _, root := range []struct {
		d     v1.Descriptor
		store *metadataStore
	}{{first, a}, {second, b}} {
		raw, err := content.FetchAll(ctx, root.store, root.d)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := bundle.ParseEnvelope(root.d, raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, child := range envelope.Children {
			if _, err := content.FetchAll(ctx, plan.graph, child); err != nil {
				t.Fatalf("lost pinned store for %s: %v", child.Digest, err)
			}
		}
	}
}

func TestDirectDefaultPolicyUsesStoreReferrerCapability(t *testing.T) {
	source, name, service := testRegistry(t)
	subject := object(t, source.store, "subject")
	dependency := object(t, source.store, "dependency")
	ref := refIndex(t, source, subject, []v1.Descriptor{dependency})
	remote, err := service.repository(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(ctx, TransferRequest{Roots: []Root{{Repository: name, Descriptor: subject, Store: remote}}, Destination: strings.TrimSuffix(name, "/test/artifacts") + "/release"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.View().Referrers) != 1 || plan.View().Referrers[0].Descriptor.Digest != ref.Digest {
		t.Fatal("default all-referrers policy lost")
	}
	if len(plan.View().Relocations) != 3 {
		t.Fatal("referrer dependency closure lost")
	}
}

// directRegistry supplies independent repository links and deliberately permits
// injecting a manifest failure after earlier graphs have been confirmed.
type directRegistry struct {
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
	reject  digest.Digest
	uploads int
}

func (r *directRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	path := strings.TrimPrefix(req.URL.Path, "/v2/")
	if i := strings.Index(path, "/blobs/uploads/"); i >= 0 {
		base := path[:i]
		if req.Method == http.MethodPost {
			w.Header().Set("Location", "/v2/"+base+"/blobs/uploads/one")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == http.MethodPut {
			raw, _ := io.ReadAll(req.Body)
			d := req.URL.Query().Get("digest")
			if digest.FromBytes(raw).String() != d {
				http.Error(w, "digest mismatch", 400)
				return
			}
			r.objects[base+"|blobs|"+d] = raw
			r.uploads++
			w.Header().Set("Docker-Content-Digest", d)
			w.Header().Set("Location", "/v2/"+base+"/blobs/"+d)
			w.WriteHeader(http.StatusCreated)
			return
		}
	}
	for _, kind := range []string{"manifests", "blobs"} {
		marker := "/" + kind + "/"
		i := strings.Index(path, marker)
		if i < 0 {
			continue
		}
		repo, reference := path[:i], path[i+len(marker):]
		key := repo + "|" + kind + "|" + reference
		if kind == "manifests" && req.Method == http.MethodPut {
			raw, _ := io.ReadAll(req.Body)
			d := digest.FromBytes(raw)
			if d == r.reject {
				http.Error(w, "injected forbidden", http.StatusForbidden)
				return
			}
			r.objects[key] = raw
			r.types[key] = req.Header.Get("Content-Type")
			digestKey := repo + "|manifests|" + d.String()
			r.objects[digestKey] = raw
			r.types[digestKey] = req.Header.Get("Content-Type")
			r.uploads++
			w.Header().Set("Docker-Content-Digest", d.String())
			w.Header().Set("Location", "/v2/"+repo+"/manifests/"+reference)
			w.WriteHeader(http.StatusCreated)
			return
		}
		raw, ok := r.objects[key]
		if !ok {
			http.NotFound(w, req)
			return
		}
		mt := r.types[key]
		if mt == "" {
			mt = "application/octet-stream"
		}
		w.Header().Set("Content-Type", mt)
		w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
		w.Header().Set("Docker-Content-Digest", digest.FromBytes(raw).String())
		if req.Method != http.MethodHead {
			_, _ = w.Write(raw)
		}
		return
	}
	http.NotFound(w, req)
}

func TestDirectExecutePartialReportsRetryAndConfirmedAliases(t *testing.T) {
	source := newMetadataStore()
	a, b := object(t, source, "a"), object(t, source, "b")
	roots := []v1.Descriptor{a, b}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Digest < roots[j].Digest })
	target := &directRegistry{objects: map[string][]byte{}, types: map[string]string{}, reject: roots[1].Digest}
	server := httptest.NewServer(http.HandlerFunc(target.serve))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	events := []Progress{}
	service, err := NewService(Options{PlainHTTPRegistries: []string{host}, OnProgress: func(p Progress) { events = append(events, p) }})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(ctx, TransferRequest{Roots: []Root{
		{Repository: "offline.example/components", Descriptor: a, Store: source, Aliases: []string{"a"}, Referrers: policy("none")},
		{Repository: "offline.example/components", Descriptor: b, Store: source, Aliases: []string{"b"}, Referrers: policy("none")},
	}, Destination: host + "/delivery", RepositoryMap: map[string]string{"offline.example/components": "app"}})
	if err != nil {
		t.Fatal(err)
	}
	partial, err := service.Execute(ctx, plan)
	if err == nil {
		t.Fatal("expected injected failure")
	}
	if errors.Is(err, ErrIntegrity) || errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
		t.Fatalf("misclassified authorization failure: %v", err)
	}
	if len(partial.Relocations) != 1 || partial.Relocations[0].Descriptor.Digest != roots[0].Digest || len(partial.Tags) != 0 {
		t.Fatalf("partial report fabricated/lost confirmations: %+v", partial)
	}
	if partial.Stats.BlobBytesDownloaded == 0 || partial.Stats.BlobBytesUploaded == 0 {
		t.Fatalf("partial stats lost: %+v", partial.Stats)
	}
	target.mu.Lock()
	target.reject = ""
	target.mu.Unlock()
	complete, err := service.Execute(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(complete.Relocations) != 2 || len(complete.Tags) != 2 {
		t.Fatalf("incomplete report: %+v", complete)
	}
	retry, err := service.Execute(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Stats.BlobBytesDownloaded != 0 || retry.Stats.BlobBytesUploaded != 0 || retry.Stats.ManifestBytesUploaded != 0 {
		t.Fatalf("repeat accounting accumulated prior runs: %+v", retry.Stats)
	}
	if len(events) == 0 {
		t.Fatal("missing synchronous progress")
	}
}

func TestDirectPolicyNormalizationDeduplicatesAndValidatesBeforeReads(t *testing.T) {
	source, name, service := testRegistry(t)
	subject := object(t, source.store, "subject")
	ref := refIndex(t, source, subject, []v1.Descriptor{})
	remote, err := service.repository(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	request := TransferRequest{Roots: []Root{{Repository: name, Descriptor: subject, Store: remote, Referrers: policy("none", ref)}}, Destination: strings.TrimSuffix(name, "/test/artifacts") + "/release"}
	unique, err := service.Plan(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Roots[0].Referrers = policy("none", ref, ref)
	duplicate, err := service.Plan(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.graph.stats.ManifestBytesDownloaded != unique.graph.stats.ManifestBytesDownloaded {
		t.Fatal("duplicate policy caused extra source reads")
	}
	if len(duplicate.View().Referrers) != 1 {
		t.Fatal("duplicate selected association")
	}
	// Any use of the source triggers an error. Invalid policy must be rejected
	// before either source store or provider is consulted.
	forbidden := &unreadableStore{}
	request.Roots[0].Store = forbidden
	request.Roots[0].ReferrerSource = nil
	request.Roots[0].Referrers = config.Policy{Include: "none", Require: []config.RequiredReferrer{{Digest: digest.Digest("invalid")}}}
	if _, err := service.Plan(ctx, request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid required digest: %v", err)
	}
	if forbidden.reads != 0 {
		t.Fatal("invalid policy accessed source")
	}
	request.Roots[0].Referrers = policy("none")
	request.Roots[0].Descriptor.Digest = digest.Digest("invalid")
	if _, err := service.Plan(ctx, request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid root digest: %v", err)
	}
	request.Roots[0].Descriptor = subject
	request.Roots[0].Repository = "relative/components"
	if _, err := service.Plan(ctx, request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("noncanonical source repository: %v", err)
	}
	if forbidden.reads != 0 {
		t.Fatal("invalid root identity accessed source")
	}
}

type unreadableStore struct{ reads int }

func (s *unreadableStore) Fetch(context.Context, v1.Descriptor) (io.ReadCloser, error) {
	s.reads++
	return nil, errors.New("unexpected source read")
}
func (s *unreadableStore) Exists(context.Context, v1.Descriptor) (bool, error) {
	s.reads++
	return false, errors.New("unexpected source check")
}

func TestDirectPlanUnionsSharedDescendantReferrersFromDistinctProviders(t *testing.T) {
	a, b := newMetadataStore(), newMetadataStore()
	shared := object(t, a, "shared image")
	if other := object(t, b, "shared image"); other.Digest != shared.Digest {
		t.Fatal("fixture not shared")
	}
	roots := []v1.Descriptor{}
	attachments := []v1.Descriptor{}
	providers := []*fixedReferrers{}
	for i, store := range []*metadataStore{a, b} {
		dependency := object(t, store, fmt.Sprint("dependency ", i))
		raw, _ := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Subject: &shared, ArtifactType: "application/example.dependencies", Manifests: []v1.Descriptor{dependency}})
		attachment := put(t, store, v1.MediaTypeImageIndex, raw)
		attachments = append(attachments, attachment)
		providers = append(providers, &fixedReferrers{refs: map[digest.Digest][]v1.Descriptor{shared.Digest: {attachment}}, reads: map[digest.Digest]int{}})
		raw, _ = json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Annotations: map[string]string{"root": fmt.Sprint(i)}, Manifests: []v1.Descriptor{shared}})
		roots = append(roots, put(t, store, v1.MediaTypeImageIndex, raw))
	}
	_, destination, service := testRegistry(t)
	plan, err := service.Plan(ctx, TransferRequest{Roots: []Root{
		{Repository: "offline.example/components", Descriptor: roots[0], Store: a, ReferrerSource: providers[0]},
		{Repository: "offline.example/components", Descriptor: roots[1], Store: b, ReferrerSource: providers[1]},
	}, Destination: destination + "/delivery"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.View().Referrers) != 2 {
		t.Fatalf("lost provider-specific referrers: %+v", plan.View().Referrers)
	}
	for i, provider := range providers {
		if provider.reads[shared.Digest] != 1 {
			t.Fatalf("provider %d consulted %d times for shared child", i, provider.reads[shared.Digest])
		}
	}
	captured := memory.New()
	for _, entry := range plan.View().Repositories {
		for _, root := range entry.Roots {
			if err := copyGraph(ctx, plan.graph, captured, root, &Stats{}, nil); err != nil {
				t.Fatal(err)
			}
			if err := bundle.VerifyClosure(ctx, captured, root); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, attachment := range attachments {
		if err := bundle.VerifyClosure(ctx, captured, attachment); err != nil {
			t.Fatalf("incomplete selected attachment: %v", err)
		}
	}
}

type fixedReferrers struct {
	refs  map[digest.Digest][]v1.Descriptor
	reads map[digest.Digest]int
}

func (p *fixedReferrers) Referrers(_ context.Context, d v1.Descriptor, _ string, fn func([]v1.Descriptor) error) error {
	p.reads[d.Digest]++
	return fn(p.refs[d.Digest])
}

func TestDirectPlanExplicitRegistryOnlyDestination(t *testing.T) {
	source := memory.New()
	descriptor := object(t, source, "portable plugin")
	root := Root{Repository: "local.invalid/plugin", Descriptor: descriptor, Store: source, Aliases: []string{"v1"}, Referrers: policy("none")}
	service, err := NewService(Options{HTTPClient: &http.Client{Transport: deliveryRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"registry.example", "localhost", "localhost:5000"} {
		plan, err := service.Plan(ctx, TransferRequest{Roots: []Root{root}, Destination: host, RepositoryMap: map[string]string{root.Repository: "helm"}})
		if err != nil {
			t.Fatal(err)
		}
		view := plan.View()
		if view.RepositoryMap[root.Repository] != host+"/helm" || len(view.Tags) != 1 || view.Tags[0].Repository != host+"/helm" || view.Tags[0].Reference != "v1" {
			t.Fatalf("wrong exact repository target: %+v", view)
		}
	}
	for _, invalid := range []string{"registry", "https://registry.example", "registry.example:invalid-port", "bad host.example", "registry.example/", "registry.example/repo:tag"} {
		if _, err := service.Plan(ctx, TransferRequest{Roots: []Root{root}, Destination: invalid}); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted invalid registry prefix %q: %v", invalid, err)
		}
	}
}
