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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote/auth"
)

type sdkRegistry struct {
	mu            sync.Mutex
	objects       map[string][]byte
	types         map[string]string
	tags          map[string]string
	writes        int
	failAlias     string
	authenticated bool
	wrongBlobSize bool
	hideBlob      string
	corruptRead   string
	failRead      string
}

func (s *sdkRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.authenticated {
		user, password, ok := r.BasicAuth()
		if !ok || user != "user" || password != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(401)
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.writes++
	}
	path := strings.TrimPrefix(r.URL.Path, "/v2/")
	if at := strings.Index(path, "/blobs/uploads/"); at >= 0 {
		if r.Method == http.MethodPost {
			w.Header().Set("Location", "/v2/"+path+"id")
			w.WriteHeader(202)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		key := r.URL.Query().Get("digest")
		s.objects[key] = raw
		w.Header().Set("Docker-Content-Digest", key)
		w.Header().Set("Location", "/v2/"+path)
		w.WriteHeader(201)
		return
	}
	var reference string
	for _, kind := range []string{"/manifests/", "/blobs/"} {
		if _, value, ok := strings.Cut(path, kind); ok {
			reference = value
			break
		}
	}
	if reference == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPut {
		if reference == s.failAlias {
			http.Error(w, "injected alias failure", http.StatusForbidden)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		key := digest.FromBytes(raw).String()
		s.objects[key] = raw
		s.types[key] = r.Header.Get("Content-Type")
		s.tags[reference] = key
		w.Header().Set("Docker-Content-Digest", key)
		w.Header().Set("Location", r.URL.Path)
		w.WriteHeader(201)
		return
	}
	key := reference
	if mapped, ok := s.tags[key]; ok {
		key = mapped
	}
	if strings.Contains(path, "/blobs/") && key == s.hideBlob {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet && key == s.failRead {
		http.Error(w, "injected read failure", http.StatusForbidden)
		return
	}
	raw, ok := s.objects[key]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Docker-Content-Digest", key)
	size := len(raw)
	if r.Method == http.MethodHead && strings.Contains(path, "/blobs/") && s.wrongBlobSize {
		size++
	}
	w.Header().Set("Content-Length", fmt.Sprint(size))
	if r.Method == http.MethodGet && key == s.corruptRead {
		raw = bytes.Repeat([]byte("!"), len(raw))
	}
	if mt := s.types[key]; mt != "" {
		w.Header().Set("Content-Type", mt)
	}
	if r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}
}
func sdkFixture(t *testing.T) bale.Root {
	t.Helper()
	store := memory.New()
	ctx := context.Background()
	put := func(mt string, raw []byte) v1.Descriptor {
		d := v1.Descriptor{MediaType: mt, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
		if err := store.Push(ctx, d, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		return d
	}
	cfg := put(v1.MediaTypeEmptyJSON, []byte("{}"))
	payload := put("application/example.payload", []byte("payload"))
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{payload}})
	if err != nil {
		t.Fatal(err)
	}
	root := put(v1.MediaTypeImageManifest, raw)
	return bale.Root{Repository: "source.example/component", Descriptor: root, Store: store, Aliases: []string{"a", "b"}, Referrers: bale.ReferrerPolicy{Include: "none"}}
}
func newSDKRegistry() *sdkRegistry {
	return &sdkRegistry{objects: map[string][]byte{}, types: map[string]string{}, tags: map[string]string{}}
}

func TestPublicClientTLSAuthPartialRetry(t *testing.T) {
	registry := newSDKRegistry()
	registry.authenticated = true
	registry.failAlias = "b"
	server := httptest.NewTLSServer(registry)
	defer server.Close()
	httpClient := server.Client()
	originalTransport := httpClient.Transport
	var credentials atomic.Int32
	var callbackActive atomic.Int32
	var overlap atomic.Bool
	var events atomic.Int32
	client, err := bale.NewClient(bale.ClientOptions{HTTPClient: httpClient, Credentials: func(_ context.Context, host string) (auth.Credential, error) {
		if host != strings.TrimPrefix(server.URL, "https://") {
			t.Errorf("unexpected credential scope %q", host)
		}
		credentials.Add(1)
		return auth.Credential{Username: "user", Password: "secret"}, nil
	}, OnProgress: func(bale.Progress) {
		if callbackActive.Add(1) != 1 {
			overlap.Store(true)
		}
		events.Add(1)
		runtime.Gosched()
		callbackActive.Add(-1)
	}})
	if err != nil {
		t.Fatal(err)
	}
	root := sdkFixture(t)
	plan, err := client.Plan(context.Background(), bale.TransferRequest{Roots: []bale.Root{root}, Destination: strings.TrimPrefix(server.URL, "https://") + "/target"})
	if err != nil {
		t.Fatal(err)
	}
	registry.mu.Lock()
	writes := registry.writes
	registry.mu.Unlock()
	if writes != 0 {
		t.Fatalf("Plan wrote %d requests", writes)
	}
	result, err := client.Execute(context.Background(), plan)
	if err == nil || len(result.Tags) != 1 || result.Tags[0].Reference != "a" {
		t.Fatalf("untruthful partial tags: %+v err=%v", result, err)
	}
	relocation, ok := result.Relocations.Lookup(bale.ArtifactID{Repository: root.Repository, Digest: root.Descriptor.Digest})
	if !ok || relocation.Destination.Digest != root.Descriptor.Digest {
		t.Fatalf("verified root relocation missing: %+v", result.Relocations)
	}
	registry.mu.Lock()
	registry.failAlias = ""
	registry.mu.Unlock()
	result, err = client.Execute(context.Background(), plan)
	if err != nil || len(result.Tags) != 2 || result.Stats.BlobBytesUploaded != 0 || result.Stats.ManifestBytesUploaded != 0 {
		t.Fatalf("retry result=%+v err=%v", result, err)
	}
	failures := make(chan error, 2)
	for range 2 {
		go func() { _, err := client.Execute(context.Background(), plan); failures <- err }()
	}
	for range 2 {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	if credentials.Load() == 0 || events.Load() == 0 || overlap.Load() {
		t.Fatalf("credentials/events/overlap: %d/%d/%v", credentials.Load(), events.Load(), overlap.Load())
	}
	if httpClient.Transport != originalTransport {
		t.Fatal("SDK mutated caller HTTP client")
	}
	// A default TLS client must still reject this private certificate.
	anonymous, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := anonymous.Resolve(context.Background(), strings.TrimPrefix(server.URL, "https://")+"/target/component:a"); err == nil {
		t.Fatal("default client trusted caller-only certificate")
	}
}

func TestPublicClientScopedHTTPAndCancellation(t *testing.T) {
	registry := newSDKRegistry()
	server := httptest.NewServer(registry)
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events int
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}, OnProgress: func(p bale.Progress) {
		events++
		if p.Operation == "tagged" {
			cancel()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	root := sdkFixture(t)
	plan, err := client.Plan(ctx, bale.TransferRequest{Roots: []bale.Root{root}, Destination: host + "/target"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Execute(ctx, plan)
	if !errors.Is(err, context.Canceled) || len(result.Tags) != 1 || events == 0 {
		t.Fatalf("cancellation lost confirmed progress: result=%+v err=%v events=%d", result, err, events)
	}
	unscoped, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{"localhost:1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unscoped.Resolve(context.Background(), host+"/target/component:a"); err == nil {
		t.Fatal("plain HTTP permission leaked to unlisted registry")
	}
}

func TestPublicClientDescriptorErrors(t *testing.T) {
	registry := newSDKRegistry()
	server := httptest.NewServer(registry)
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	root := sdkFixture(t)
	request := bale.TransferRequest{Roots: []bale.Root{root}, Destination: host + "/target"}
	plan, err := client.Plan(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Execute(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	registry.mu.Lock()
	registry.wrongBlobSize = true
	registry.mu.Unlock()
	if _, err := client.Execute(context.Background(), plan); !errors.Is(err, bale.ErrIntegrity) {
		t.Fatalf("execute size mismatch not typed integrity: %v", err)
	}
	remoteRoot, err := client.Resolve(context.Background(), host+"/target/component:a")
	if err != nil {
		t.Fatal(err)
	}
	remoteRoot.Referrers = bale.ReferrerPolicy{Include: "none"}
	if _, err := client.Plan(context.Background(), bale.TransferRequest{Roots: []bale.Root{remoteRoot}, Destination: host + "/other"}); !errors.Is(err, bale.ErrIntegrity) {
		t.Fatalf("plan size mismatch not typed integrity: %v", err)
	}
	registry.mu.Lock()
	registry.wrongBlobSize = false
	registry.mu.Unlock()
	raw, err := content.FetchAll(context.Background(), root.Store, root.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	registry.mu.Lock()
	registry.hideBlob = manifest.Layers[0].Digest.String()
	registry.mu.Unlock()
	if _, err := client.Execute(context.Background(), plan); !errors.Is(err, bale.ErrNotFound) {
		t.Fatalf("missing closure payload not typed not-found: %v", err)
	}
	// A denied verification read remains an authorization error, not corruption.
	registry.mu.Lock()
	registry.hideBlob = ""
	registry.failRead = root.Descriptor.Digest.String()
	registry.mu.Unlock()
	if _, err := client.Execute(context.Background(), plan); err == nil || errors.Is(err, bale.ErrIntegrity) {
		t.Fatalf("read denial classified as corruption: %v", err)
	}
}
