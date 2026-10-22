package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

type corruptAttachmentStore struct{ content.ReadOnlyStorage }

func (s corruptAttachmentStore) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	raw, err := content.FetchAll(ctx, s.ReadOnlyStorage, d)
	if err != nil {
		return nil, err
	}
	raw[0] = '!'
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func attachmentFixture(t *testing.T, subject *v1.Descriptor, index bool, id string) bale.AttachRequest {
	t.Helper()
	store := memory.New()
	put := func(mt string, raw []byte) v1.Descriptor {
		d := v1.Descriptor{MediaType: mt, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
		if err := store.Push(context.Background(), d, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		return d
	}
	var value any
	mt := v1.MediaTypeImageManifest
	if index {
		mt = v1.MediaTypeImageIndex
		value = v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: mt, ArtifactType: "application/example.index", Subject: subject, Manifests: []v1.Descriptor{}, Annotations: map[string]string{"id": id}}
	} else {
		cfg := put("application/example.config", []byte("{}"))
		payload := put("application/example.payload", []byte(id))
		// Deliberately omit artifactType: OCI referrers derive it from config.
		value = v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: mt, Config: cfg, Layers: []v1.Descriptor{payload}, Subject: subject}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return bale.AttachRequest{Descriptor: put(mt, raw), Store: store}
}

func attachmentSubject(t *testing.T, client *bale.Client, host string) bale.Root {
	t.Helper()
	plan, err := client.Plan(context.Background(), bale.TransferRequest{Roots: []bale.Root{sdkFixture(t)}, Destination: host + "/target"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Execute(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	root, err := client.Resolve(context.Background(), host+"/target/component:a")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func referrerDigests(refs []v1.Descriptor) []string {
	result := make([]string, len(refs))
	for i, ref := range refs {
		result[i] = ref.Digest.String()
	}
	sort.Strings(result)
	return result
}

func TestPublicAttachFallbackRepairAndPreservation(t *testing.T) {
	ctx := context.Background()
	registry := newSDKRegistry()
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
		}
		registry.ServeHTTP(w, r)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	subject := attachmentSubject(t, client, host)
	first := attachmentFixture(t, &subject.Descriptor, false, "first")
	second := attachmentFixture(t, &subject.Descriptor, true, "second")
	first.Subject, second.Subject = subject.Repository+":a", subject.Repository+":a"
	for _, request := range []bale.AttachRequest{first, second} {
		result, err := client.Attach(ctx, request)
		if err != nil || !result.Published || !result.Discoverable || result.Subject.Digest != subject.Descriptor.Digest || result.Descriptor.Digest != request.Descriptor.Digest {
			t.Fatalf("attach result=%+v err=%v", result, err)
		}
	}
	refs, err := client.ListReferrers(ctx, first.Subject, bale.ListReferrersOptions{})
	want := referrerDigests([]v1.Descriptor{first.Descriptor, second.Descriptor})
	if err != nil || !reflect.DeepEqual(referrerDigests(refs), want) {
		t.Fatalf("refs=%+v err=%v", refs, err)
	}
	filtered, err := client.ListReferrers(ctx, first.Subject, bale.ListReferrersOptions{ArtifactType: "application/example.config"})
	if err != nil || len(filtered) != 1 || filtered[0].Digest != first.Descriptor.Digest {
		t.Fatalf("effective artifact type filter: %+v err=%v", filtered, err)
	}
	for _, request := range []bale.AttachRequest{first, second} {
		result, err := client.Attach(ctx, request)
		if err != nil || !result.Discoverable || result.Stats.ManifestBytesUploaded != 0 || result.Stats.BlobBytesUploaded != 0 {
			t.Fatalf("healthy retry=%+v err=%v", result, err)
		}
	}
	// Simulate an interrupted inventory update: the first root is still present,
	// while only the second entry remains in the fallback inventory.
	managedTag := strings.Replace(subject.Descriptor.Digest.String(), ":", "-", 1)
	registry.mu.Lock()
	oldIndexDigest := registry.tags[managedTag]
	oldIndex := append([]byte(nil), registry.objects[oldIndexDigest]...)
	var index v1.Index
	if err := json.Unmarshal(oldIndex, &index); err != nil {
		registry.mu.Unlock()
		t.Fatal(err)
	}
	index.Manifests = nil
	for _, ref := range refs {
		if ref.Digest == second.Descriptor.Digest {
			index.Manifests = append(index.Manifests, ref)
		}
	}
	raw, err := json.Marshal(index)
	if err != nil {
		registry.mu.Unlock()
		t.Fatal(err)
	}
	key := digest.FromBytes(raw).String()
	registry.objects[key], registry.types[key], registry.tags[managedTag] = raw, v1.MediaTypeImageIndex, key
	registry.mu.Unlock()
	result, err := client.Attach(ctx, first)
	if err != nil || !result.Published || !result.Discoverable || result.Stats.ManifestBytesUploaded != first.Descriptor.Size || result.Stats.BlobBytesUploaded != 0 {
		t.Fatalf("repair=%+v err=%v", result, err)
	}
	refs, err = client.ListReferrers(ctx, first.Subject, bale.ListReferrersOptions{})
	if err != nil || !reflect.DeepEqual(referrerDigests(refs), want) {
		t.Fatalf("repair lost inventory: %+v err=%v", refs, err)
	}
	registry.mu.Lock()
	preserved := bytes.Equal(registry.objects[oldIndexDigest], oldIndex)
	registry.mu.Unlock()
	if !preserved || deletes.Load() != 0 {
		t.Fatalf("old inventory preserved=%v deletes=%d", preserved, deletes.Load())
	}
}

func TestPublicAttachValidationBeforeWrites(t *testing.T) {
	registry := newSDKRegistry()
	server := httptest.NewServer(registry)
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	subject := attachmentSubject(t, client, host)
	wrong := subject.Descriptor
	wrong.Size++
	unsupported := attachmentFixture(t, &subject.Descriptor, false, "unsupported")
	unsupported.Descriptor.MediaType = "application/vnd.docker.distribution.manifest.v2+json"
	missing := attachmentFixture(t, &subject.Descriptor, false, "missing child")
	rootBytes, err := content.FetchAll(context.Background(), missing.Store, missing.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	rootOnly := memory.New()
	if err := rootOnly.Push(context.Background(), missing.Descriptor, bytes.NewReader(rootBytes)); err != nil {
		t.Fatal(err)
	}
	missing.Store = rootOnly
	corrupt := attachmentFixture(t, &subject.Descriptor, false, "corrupt")
	corrupt.Store = corruptAttachmentStore{corrupt.Store}
	invalid := attachmentFixture(t, &subject.Descriptor, false, "invalid")
	invalid.Descriptor.Digest = "not-a-digest"
	cases := []struct {
		name    string
		request bale.AttachRequest
		want    error
	}{
		{"missing store", bale.AttachRequest{Descriptor: subject.Descriptor}, bale.ErrInvalid},
		{"unsupported", unsupported, bale.ErrUnsupported},
		{"no subject", attachmentFixture(t, nil, false, "none"), bale.ErrInvalid},
		{"wrong subject", attachmentFixture(t, &wrong, true, "wrong"), bale.ErrInvalid},
		{"missing child", missing, bale.ErrNotFound},
		{"corrupt root", corrupt, bale.ErrIntegrity},
		{"invalid descriptor", invalid, bale.ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.request.Subject = subject.Repository + ":a"
			registry.mu.Lock()
			before := registry.writes
			registry.mu.Unlock()
			result, err := client.Attach(context.Background(), tc.request)
			if !errors.Is(err, tc.want) || result.Published || result.Discoverable {
				t.Fatalf("result=%+v err=%v; want %v", result, err, tc.want)
			}
			registry.mu.Lock()
			after := registry.writes
			registry.mu.Unlock()
			if before != after {
				t.Fatalf("invalid request wrote %d requests", after-before)
			}
		})
	}
}

func TestPublicAttachInventoryFailureRetainsPublication(t *testing.T) {
	registry := newSDKRegistry()
	server := httptest.NewServer(registry)
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	subject := attachmentSubject(t, client, host)
	request := attachmentFixture(t, &subject.Descriptor, false, "inventory-failure")
	request.Subject = subject.Repository + ":a"
	registry.mu.Lock()
	registry.failAlias = strings.Replace(subject.Descriptor.Digest.String(), ":", "-", 1)
	registry.mu.Unlock()
	result, err := client.Attach(context.Background(), request)
	if err == nil || !result.Published || result.Discoverable || errors.Is(err, bale.ErrIntegrity) || errors.Is(err, bale.ErrNotFound) {
		t.Fatalf("inventory failure must retain publication and original forbidden error: %+v err=%v", result, err)
	}
	registry.mu.Lock()
	registry.failAlias = ""
	registry.mu.Unlock()
	result, err = client.Attach(context.Background(), request)
	if err != nil || !result.Published || !result.Discoverable || result.Stats.BlobBytesUploaded != 0 || result.Stats.ManifestBytesUploaded != request.Descriptor.Size {
		t.Fatalf("retry after inventory failure: %+v err=%v", result, err)
	}
}

func TestPublicAttachPinsSubjectAndWaitIsCancelable(t *testing.T) {
	registry := newSDKRegistry()
	server := httptest.NewServer(registry)
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	published, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}, OnProgress: func(event bale.Progress) {
		if event.Operation == "attachment-published" {
			once.Do(func() { close(published); <-release })
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	subject := attachmentSubject(t, client, host)
	first := attachmentFixture(t, &subject.Descriptor, true, "first")
	first.Subject = subject.Repository + ":a"
	done := make(chan error, 1)
	go func() { _, err := client.Attach(context.Background(), first); done <- err }()
	select {
	case <-published:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("attachment never published")
	}
	ctx, cancel := context.WithCancel(context.Background())
	second := attachmentFixture(t, &subject.Descriptor, true, "second")
	second.Subject = first.Subject
	waiting := make(chan error, 1)
	go func() { _, err := client.Attach(ctx, second); waiting <- err }()
	cancel()
	select {
	case err := <-waiting:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waiting cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("canceled Attach blocked behind another call")
	}
	// Move the mutable tag after publication. Discovery must still use the
	// original pinned subject rather than resolving the tag a second time.
	registry.mu.Lock()
	registry.tags["a"] = first.Descriptor.Digest.String()
	registry.mu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	refs, err := client.ListReferrers(context.Background(), subject.Repository+"@"+subject.Descriptor.Digest.String(), bale.ListReferrersOptions{})
	if err != nil || len(refs) != 1 || refs[0].Digest != first.Descriptor.Digest {
		t.Fatalf("pinned discovery: %+v err=%v", refs, err)
	}
}

func TestPublicListReferrersValidatesPagesBeforeFiltering(t *testing.T) {
	ctx := context.Background()
	registry := newSDKRegistry()
	var pagesMu sync.Mutex
	var pages [][]v1.Descriptor
	var requestedPages atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/referrers/") {
			requestedPages.Add(1)
			pagesMu.Lock()
			defer pagesMu.Unlock()
			page := 0
			if r.URL.Query().Get("page") == "2" {
				page = 1
			}
			if page == 0 && len(pages) > 1 {
				w.Header().Set("Link", fmt.Sprintf("<%s?page=2>; rel=\"next\"", r.URL.Path))
			}
			w.Header().Set("Content-Type", v1.MediaTypeImageIndex)
			refs := []v1.Descriptor{}
			if page < len(pages) {
				refs = pages[page]
			}
			_ = json.NewEncoder(w).Encode(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: refs})
			return
		}
		registry.ServeHTTP(w, r)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	subject := attachmentSubject(t, client, host)
	first := attachmentFixture(t, &subject.Descriptor, false, "first")
	second := attachmentFixture(t, &subject.Descriptor, true, "second")
	// Seed actual roots separately from the advertised inventory. A native
	// registry returning a hint does not establish that the root is published.
	for _, request := range []bale.AttachRequest{first, second} {
		raw, err := content.FetchAll(ctx, request.Store, request.Descriptor)
		if err != nil {
			t.Fatal(err)
		}
		registry.mu.Lock()
		registry.objects[request.Descriptor.Digest.String()] = raw
		registry.types[request.Descriptor.Digest.String()] = request.Descriptor.MediaType
		registry.mu.Unlock()
	}
	first.Descriptor.ArtifactType = "application/example.config"
	second.Descriptor.ArtifactType = "application/example.index"
	pagesMu.Lock()
	pages = [][]v1.Descriptor{{second.Descriptor}, {first.Descriptor, second.Descriptor}}
	pagesMu.Unlock()
	refs, err := client.ListReferrers(ctx, subject.Repository+":a", bale.ListReferrersOptions{})
	if err != nil || len(refs) != 2 || !sort.SliceIsSorted(refs, func(i, j int) bool { return refs[i].Digest.String() < refs[j].Digest.String() }) || requestedPages.Load() != 2 {
		t.Fatalf("paginated refs=%+v requests=%d err=%v", refs, requestedPages.Load(), err)
	}
	wrongType := first.Descriptor
	wrongType.ArtifactType = "application/example.forged"
	pagesMu.Lock()
	pages = [][]v1.Descriptor{{second.Descriptor}, {wrongType}}
	pagesMu.Unlock()
	refs, err = client.ListReferrers(ctx, subject.Repository+":a", bale.ListReferrersOptions{ArtifactType: second.Descriptor.ArtifactType})
	if !errors.Is(err, bale.ErrIntegrity) || len(refs) != 1 || refs[0].Digest != second.Descriptor.Digest {
		t.Fatalf("filtered malformed later page must fail with prior verified results: %+v err=%v", refs, err)
	}
	// Inline bytes must never turn an unpublished inventory entry into a root.
	first.Descriptor.Data, err = content.FetchAll(ctx, first.Store, first.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	registry.mu.Lock()
	delete(registry.objects, first.Descriptor.Digest.String())
	registry.mu.Unlock()
	pagesMu.Lock()
	pages = [][]v1.Descriptor{{first.Descriptor}}
	pagesMu.Unlock()
	refs, err = client.ListReferrers(ctx, subject.Repository+":a", bale.ListReferrersOptions{})
	if !errors.Is(err, bale.ErrNotFound) || len(refs) != 0 {
		t.Fatalf("unpublished inline hint accepted: %+v err=%v", refs, err)
	}
}

func TestPublicListReferrersRejectsMalformedInventory(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, tc := range []struct{ name, body, mediaType string }{
			{"wrong schema", `{"schemaVersion":3,"manifests":[]}`, v1.MediaTypeImageIndex},
			{"missing manifests", `{"schemaVersion":2}`, v1.MediaTypeImageIndex},
			{"null manifests", `{"schemaVersion":2,"manifests":null}`, v1.MediaTypeImageIndex},
			{"duplicate fields", `{"schemaVersion":2,"manifests":[],"manifests":[]}`, v1.MediaTypeImageIndex},
			{"nonindex", `{}`, v1.MediaTypeImageManifest},
		} {
			t.Run(fmt.Sprintf("native=%v/%s", native, tc.name), func(t *testing.T) {
				registry := newSDKRegistry()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if native && strings.Contains(r.URL.Path, "/referrers/") {
						w.Header().Set("Content-Type", tc.mediaType)
						_, _ = io.WriteString(w, tc.body)
						return
					}
					registry.ServeHTTP(w, r)
				}))
				defer server.Close()
				host := strings.TrimPrefix(server.URL, "http://")
				client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
				if err != nil {
					t.Fatal(err)
				}
				subject := attachmentSubject(t, client, host)
				if !native {
					registry.mu.Lock()
					key := digest.FromString(tc.body).String()
					registry.objects[key] = []byte(tc.body)
					registry.types[key] = tc.mediaType
					registry.tags[strings.Replace(subject.Descriptor.Digest.String(), ":", "-", 1)] = key
					registry.mu.Unlock()
				}
				refs, err := client.ListReferrers(context.Background(), subject.Repository+":a", bale.ListReferrersOptions{})
				if !errors.Is(err, bale.ErrIntegrity) || len(refs) != 0 {
					t.Fatalf("malformed inventory accepted: %+v err=%v", refs, err)
				}
			})
		}
	}
}

func TestPublicListReferrersEmptyIndexSubject(t *testing.T) {
	// An empty native inventory can have the same bytes as an empty-index
	// subject. A fallback tag pointing at that subject is a conflicting tag.
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			registry := newSDKRegistry()
			raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
			d := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
			registry.objects[d.Digest.String()], registry.types[d.Digest.String()], registry.tags["subject"] = raw, d.MediaType, d.Digest.String()
			registry.tags[strings.Replace(d.Digest.String(), ":", "-", 1)] = d.Digest.String()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if native && strings.Contains(r.URL.Path, "/referrers/") {
					w.Header().Set("Content-Type", v1.MediaTypeImageIndex)
					_, _ = w.Write(raw)
					return
				}
				registry.ServeHTTP(w, r)
			}))
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "http://")
			client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
			if err != nil {
				t.Fatal(err)
			}
			refs, err := client.ListReferrers(context.Background(), host+"/repo:subject", bale.ListReferrersOptions{})
			if len(refs) != 0 || (native && err != nil) || (!native && !errors.Is(err, bale.ErrIntegrity)) {
				t.Fatalf("empty index subject native=%v refs=%+v err=%v", native, refs, err)
			}
		})
	}
}

func TestPublicAttachConcurrentCallsPreserveInventory(t *testing.T) {
	registry := newSDKRegistry()
	server := httptest.NewServer(registry)
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	subject := attachmentSubject(t, client, host)
	first := attachmentFixture(t, &subject.Descriptor, true, "first")
	second := attachmentFixture(t, &subject.Descriptor, true, "second")
	first.Subject, second.Subject = subject.Repository+":a", subject.Repository+":a"
	start := make(chan struct{})
	done := make(chan error, 2)
	for _, request := range []bale.AttachRequest{first, second} {
		go func() { <-start; _, err := client.Attach(context.Background(), request); done <- err }()
	}
	close(start)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	refs, err := client.ListReferrers(context.Background(), first.Subject, bale.ListReferrersOptions{})
	if err != nil || !reflect.DeepEqual(referrerDigests(refs), referrerDigests([]v1.Descriptor{first.Descriptor, second.Descriptor})) {
		t.Fatalf("concurrent inventory=%+v err=%v", refs, err)
	}
}
