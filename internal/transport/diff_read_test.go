package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/auth"
)

type diffGuardStore struct {
	content.ReadOnlyStorage
	blocked     map[digest.Digest]bool
	blockedType string
	corruptType string
	exists      int
	opaqueReads int
	onFetch     func()
}

func (s *diffGuardStore) Exists(context.Context, v1.Descriptor) (bool, error) {
	s.exists++
	return false, errors.New("unexpected payload existence check")
}
func (s *diffGuardStore) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	if s.onFetch != nil {
		s.onFetch()
	}
	if s.blocked[d.Digest] {
		s.opaqueReads++
		return nil, errors.New("unexpected opaque fetch")
	}
	if d.MediaType == s.blockedType {
		return nil, errdef.ErrNotFound
	}
	if d.MediaType == s.corruptType {
		return io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), int(d.Size)))), nil
	}
	return s.ReadOnlyStorage.Fetch(ctx, d)
}

func diffFixture(t *testing.T) (*Service, *memory.Store, bundle.Snapshot, []v1.Descriptor) {
	t.Helper()
	store := memory.New()
	cfg := put(t, store, "application/example.config", []byte("opaque config"))
	payload := put(t, store, "application/example.payload", []byte("opaque payload"))
	raw, err := jsonManifest(cfg, payload)
	if err != nil {
		t.Fatal(err)
	}
	member := put(t, store, v1.MediaTypeImageManifest, raw)
	inner := snapshotFor(t, store, []string{"unreachable.invalid/member"}, []v1.Descriptor{member})
	outer := snapshotFor(t, store, []string{"unreachable.invalid/inner"}, []v1.Descriptor{inner.Descriptor})
	service, err := NewService(Options{})
	if err != nil {
		t.Fatal(err)
	}
	return service, store, outer, []v1.Descriptor{cfg, payload}
}

func jsonManifest(cfg, payload v1.Descriptor) ([]byte, error) {
	return json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{payload}})
}

func TestDiffRootsReadsOnlyMetadata(t *testing.T) {
	service, store, snapshot, opaque := diffFixture(t)
	guard := &diffGuardStore{ReadOnlyStorage: store, blocked: map[digest.Digest]bool{}}
	for _, d := range opaque {
		guard.blocked[d.Digest] = true
	}
	root := Root{Descriptor: snapshot.Descriptor, Store: guard}
	if _, err := service.DiffRoots(ctx, root, root); err != nil {
		t.Fatal(err)
	}
	if guard.exists != 0 || guard.opaqueReads != 0 {
		t.Fatalf("opaque accesses: %+v", guard)
	}
	if _, err := content.FetchAll(ctx, store, opaque[0]); err != nil {
		t.Fatalf("caller store changed: %v", err)
	}
	// Full inspection retains its stronger, complete-payload validation.
	if _, err := service.InspectRoot(ctx, root); err == nil {
		t.Fatal("full inspection no longer checks payloads")
	}
}

func TestDiffLayoutsAllowsMissingOpaquePayloads(t *testing.T) {
	service, store, snapshot, opaque := diffFixture(t)
	path := layoutFor(t, store, snapshot.Descriptor)
	for _, d := range opaque {
		if err := os.Remove(filepath.Join(path, "blobs", d.Digest.Algorithm().String(), d.Digest.Encoded())); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(filepath.Join(path, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", filepath.Join(t.TempDir(), "not-a-directory"))
	if _, err := service.DiffLayouts(ctx, path, path); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(path, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("layout mutated")
	}
	if _, err := service.InspectLayout(ctx, path); !errors.Is(err, ErrNotFound) {
		t.Fatalf("full inspect missing payload: %v", err)
	}
}

func TestDiffInvalidInputHasNoPartialResult(t *testing.T) {
	service, store, snapshot, _ := diffFixture(t)
	good := Root{Descriptor: snapshot.Descriptor, Store: store}
	for _, tc := range []struct {
		name string
		root Root
		kind error
	}{
		{"missing store", Root{Descriptor: snapshot.Descriptor}, ErrInvalid},
		{"missing metadata", Root{Descriptor: snapshot.Descriptor, Store: &diffGuardStore{ReadOnlyStorage: store, blockedType: bundle.MetadataMediaType}}, ErrNotFound},
		{"corrupt metadata", Root{Descriptor: snapshot.Descriptor, Store: &diffGuardStore{ReadOnlyStorage: store, corruptType: bundle.MetadataMediaType}}, ErrIntegrity},
		{"not a bundle", Root{Descriptor: object(t, store, "ordinary"), Store: store}, ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := service.DiffRoots(ctx, good, tc.root)
			if !errors.Is(err, tc.kind) || !strings.Contains(err.Error(), "diff after") {
				t.Fatalf("error: %v", err)
			}
			var typed *Error
			if !errors.As(err, &typed) {
				t.Fatalf("lost typed error: %v", err)
			}
			if !reflect.DeepEqual(result, DiffResult{}) {
				t.Fatalf("partial result: %+v", result)
			}
		})
	}
	path := layoutFor(t, store, snapshot.Descriptor)
	if err := os.WriteFile(filepath.Join(path, "oci-layout"), []byte(`{"imageLayoutVersion":"wrong"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.DiffLayouts(ctx, path, "unused"); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "diff before") {
		t.Fatalf("invalid framing: %v", err)
	}
	if _, err := service.DiffLayouts(ctx, "", path); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty path: %v", err)
	}
}

func TestDiffValidatesSelectedDependencyJSON(t *testing.T) {
	r, repo, service := testRegistry(t)
	subject := object(t, r.store, "subject")
	r.tags["root"] = subject
	dependencyDeclaration(t, r, subject, "production")
	captured, snapshot, err := service.capture(ctx, config.Recipe{Include: []config.Selection{dependencySelection(repo + ":root")}})
	if err != nil {
		t.Fatal(err)
	}
	store := memory.New()
	if err := copyGraph(ctx, captured, store, snapshot.Descriptor, &Stats{}, nil); err != nil {
		t.Fatal(err)
	}
	root := Root{Descriptor: snapshot.Descriptor, Store: store}
	for _, corrupt := range []bool{false, true} {
		guard := &diffGuardStore{ReadOnlyStorage: store}
		if corrupt {
			guard.corruptType = dependency.ArtifactType
		} else {
			guard.blockedType = dependency.ArtifactType
		}
		broken := Root{Descriptor: snapshot.Descriptor, Store: guard}
		result, err := service.DiffRoots(ctx, root, broken)
		if err == nil || !reflect.DeepEqual(result, DiffResult{}) {
			t.Fatalf("accepted invalid dependency JSON: %+v %v", result, err)
		}
	}
}

func TestDiffCancellation(t *testing.T) {
	service, store, snapshot, _ := diffFixture(t)
	for _, during := range []bool{false, true} {
		canceled, cancel := context.WithCancel(ctx)
		root := Root{Descriptor: snapshot.Descriptor, Store: store}
		if during {
			root.Store = &diffGuardStore{ReadOnlyStorage: store, onFetch: cancel}
		} else {
			cancel()
		}
		result, err := service.DiffRoots(canceled, root, root)
		cancel()
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, DiffResult{}) {
			t.Fatalf("cancellation: %+v %v", result, err)
		}
	}
}

func TestDiffRegistryPinsOnceAndUsesOnlyCapturedGraph(t *testing.T) {
	r, repo, service := testRegistry(t)
	subject := object(t, r.store, "opaque application")
	snapshot := snapshotFor(t, r.store, []string{"offline.invalid/source"}, []v1.Descriptor{subject})
	r.tags["before"] = snapshot.Descriptor
	r.tags["after"] = snapshot.Descriptor
	requests := []string{}
	service.client.Client = &http.Client{Transport: deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Method+" "+req.URL.Path)
		if req.URL.Host != strings.Split(repo, "/")[0] {
			return nil, fmt.Errorf("historical source contacted: %s", req.URL)
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			return nil, fmt.Errorf("unexpected write: %s", req.Method)
		}
		if strings.Contains(req.URL.Path, "/referrers/") {
			return nil, errors.New("unexpected referrer listing")
		}
		return http.DefaultTransport.RoundTrip(req)
	})}
	for _, same := range []bool{false, true} {
		r.headTags = map[string]int{}
		after := "after"
		if same {
			after = "before"
		}
		if _, err := service.Diff(ctx, repo+":before", repo+":"+after); err != nil {
			t.Fatal(err)
		}
		if r.headTags["before"] != 1 {
			t.Fatalf("before resolves: %v", r.headTags)
		}
		want := 1
		if same {
			want = 0
		}
		if r.headTags["after"] != want {
			t.Fatalf("after resolves: %v", r.headTags)
		}
	}
	if len(requests) == 0 {
		t.Fatal("no registry reads")
	}
	// The only blob GETs are interpreted Bale metadata, never the application layer.
	for _, request := range requests {
		if strings.Contains(request, "/blobs/"+digest.FromString("opaque application").String()) {
			t.Fatalf("opaque request: %s", request)
		}
	}
	failure := errors.New("network denied")
	denied, err := NewService(Options{HTTPClient: &http.Client{Transport: deliveryRoundTripper(func(*http.Request) (*http.Response, error) { return nil, failure })}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := denied.Diff(ctx, repo+":before", repo+":after"); !errors.Is(err, failure) {
		t.Fatalf("network policy lost: %v", err)
	}
	// SDK construction does not parse ambient Docker configuration.
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	service.client.Credential = func(context.Context, string) (auth.Credential, error) {
		return auth.Credential{Username: "explicit-user", Password: "explicit-password"}, nil
	}
	service.client.Client = &http.Client{Transport: deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") == "" {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Www-Authenticate": []string{`Basic realm="test"`}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		}
		if user, password, ok := req.BasicAuth(); !ok || user != "explicit-user" || password != "explicit-password" {
			return nil, errors.New("explicit credentials not used")
		}
		return http.DefaultTransport.RoundTrip(req)
	})}
	if _, err := service.Diff(ctx, repo+":before", repo+":before"); err != nil {
		t.Fatal(err)
	}
}

func TestDiffCancellationBetweenOperandsAndBeforeComparison(t *testing.T) {
	for _, cancelAfter := range []string{"before", "after"} {
		t.Run(cancelAfter, func(t *testing.T) {
			canceled, cancel := context.WithCancel(ctx)
			defer cancel()
			secondRead := false
			result, err := diffInputs(canceled, "first", "second", func() (InspectResult, error) {
				if cancelAfter == "before" {
					cancel()
				}
				return InspectResult{}, nil
			}, func() (InspectResult, error) {
				secondRead = true
				cancel()
				return InspectResult{}, nil
			})
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, DiffResult{}) {
				t.Fatalf("cancellation: %+v %v", result, err)
			}
			if cancelAfter == "before" && secondRead {
				t.Fatal("second operand read after cancellation")
			}
		})
	}
}
