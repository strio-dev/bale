package transport

import (
	"context"
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
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/deliveryfile"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote/auth"
)

func TestUpdateDeliveryRootsReadsOnlyMetadataAndIncludesNestedInventory(t *testing.T) {
	service, store, snapshot, opaque := diffFixture(t)
	guard := &diffGuardStore{ReadOnlyStorage: store, blocked: map[digest.Digest]bool{}}
	for _, descriptor := range opaque {
		guard.blocked[descriptor.Digest] = true
	}
	root := Root{Descriptor: snapshot.Descriptor, Store: guard}
	file := deliveryfile.File{Bundle: snapshot.Descriptor.Digest, Artifacts: []deliveryfile.Artifact{}}
	result, err := service.UpdateDeliveryRoots(ctx, root, root, file)
	if err != nil {
		t.Fatal(err)
	}
	if result.Delivery == nil || len(result.Delivery.Artifacts) != 2 {
		t.Fatalf("missing nested inventory: %+v", result)
	}
	for _, repo := range []string{"unreachable.invalid/inner", "unreachable.invalid/member"} {
		found := false
		for _, artifact := range result.Delivery.Artifacts {
			found = found || strings.HasPrefix(artifact.Ref, repo+"@")
		}
		if !found {
			t.Fatalf("missing %s: %+v", repo, result.Delivery)
		}
	}
	if guard.exists != 0 || guard.opaqueReads != 0 {
		t.Fatalf("opaque accesses: %+v", guard)
	}
	if _, err := content.FetchAll(ctx, store, opaque[0]); err != nil {
		t.Fatalf("caller store changed: %v", err)
	}
	if _, err := service.InspectRoot(ctx, root); err == nil {
		t.Fatal("full inspection no longer checks payloads")
	}
}

func TestUpdateDeliveryLayoutsAllowsMissingOpaquePayloadsWithoutWrites(t *testing.T) {
	service, store, snapshot, opaque := diffFixture(t)
	path := layoutFor(t, store, snapshot.Descriptor)
	for _, descriptor := range opaque {
		if err := os.Remove(filepath.Join(path, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded())); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DOCKER_CONFIG", filepath.Join(t.TempDir(), "unreadable-config"))
	before := deliveryUpdateLayoutBytes(t, path)
	file := deliveryfile.File{Bundle: snapshot.Descriptor.Digest, Artifacts: []deliveryfile.Artifact{}}
	result, err := service.UpdateDeliveryLayouts(ctx, path, path, file)
	if err != nil || result.Delivery == nil {
		t.Fatalf("update: %+v %v", result, err)
	}
	if !reflect.DeepEqual(before, deliveryUpdateLayoutBytes(t, path)) {
		t.Fatal("layout mutated")
	}
	if _, err := service.InspectLayout(ctx, path); !errors.Is(err, ErrNotFound) {
		t.Fatalf("full inspect missing payload: %v", err)
	}
}

func deliveryUpdateLayoutBytes(t *testing.T, path string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(path, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		files[name], err = os.ReadFile(name)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestUpdateDeliveryReadErrorsReturnNoPartialResult(t *testing.T) {
	service, store, snapshot, _ := diffFixture(t)
	good := Root{Descriptor: snapshot.Descriptor, Store: store}
	file := deliveryfile.File{Bundle: snapshot.Descriptor.Digest}
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
			result, err := service.UpdateDeliveryRoots(ctx, good, tc.root, file)
			var typed *Error
			if !errors.Is(err, tc.kind) || !errors.As(err, &typed) || !strings.Contains(err.Error(), "update delivery after") {
				t.Fatalf("error classification/context: %v", err)
			}
			if !reflect.DeepEqual(result, DeliveryUpdateResult{}) {
				t.Fatalf("partial result: %+v", result)
			}
		})
	}
	path := layoutFor(t, store, snapshot.Descriptor)
	if err := os.WriteFile(filepath.Join(path, "oci-layout"), []byte(`{"imageLayoutVersion":"wrong"}`), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := service.UpdateDeliveryLayouts(ctx, path, "unused", file)
	if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "update delivery before") || !reflect.DeepEqual(result, DeliveryUpdateResult{}) {
		t.Fatalf("invalid layout framing: %+v %v", result, err)
	}
}

func TestUpdateDeliveryValidatesSelectedDependencyJSON(t *testing.T) {
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
	file := deliveryfile.File{Bundle: snapshot.Descriptor.Digest}
	for _, corrupt := range []bool{false, true} {
		guard := &diffGuardStore{ReadOnlyStorage: store}
		if corrupt {
			guard.corruptType = dependency.ArtifactType
		} else {
			guard.blockedType = dependency.ArtifactType
		}
		broken := Root{Descriptor: snapshot.Descriptor, Store: guard}
		result, err := service.UpdateDeliveryRoots(ctx, root, broken, file)
		if err == nil || !reflect.DeepEqual(result, DeliveryUpdateResult{}) {
			t.Fatalf("accepted invalid interpreted JSON: %+v %v", result, err)
		}
	}
}

func TestUpdateDeliveryRegistryPinsOnceAndHonorsHTTPAndCredentials(t *testing.T) {
	r, repo, service := testRegistry(t)
	opaqueConfig := put(t, r.store, "application/example.config", []byte("opaque application config"))
	opaquePayload := put(t, r.store, "application/example.payload", []byte("opaque application"))
	raw, err := jsonManifest(opaqueConfig, opaquePayload)
	if err != nil {
		t.Fatal(err)
	}
	subject := put(t, r.store, v1.MediaTypeImageManifest, raw)
	snapshot := snapshotFor(t, r.store, []string{"offline.invalid/source"}, []v1.Descriptor{subject})
	r.tags["before"] = snapshot.Descriptor
	r.tags["after"] = snapshot.Descriptor
	file := deliveryfile.File{Bundle: snapshot.Descriptor.Digest}
	requests := []string{}
	service.client.Credential = func(context.Context, string) (auth.Credential, error) {
		return auth.Credential{Username: "explicit-user", Password: "explicit-password"}, nil
	}
	service.client.Client = &http.Client{Transport: deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Method+" "+req.URL.Path)
		if req.URL.Host != strings.Split(repo, "/")[0] || req.URL.Scheme != "http" {
			return nil, fmt.Errorf("unexpected host or protocol: %s", req.URL)
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			return nil, fmt.Errorf("unexpected write: %s", req.Method)
		}
		if strings.Contains(req.URL.Path, "/referrers/") {
			return nil, errors.New("unexpected referrer listing")
		}
		if req.Header.Get("Authorization") == "" {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Www-Authenticate": []string{`Basic realm="test"`}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		}
		if user, password, ok := req.BasicAuth(); !ok || user != "explicit-user" || password != "explicit-password" {
			return nil, errors.New("explicit credentials not used")
		}
		return http.DefaultTransport.RoundTrip(req)
	})}
	for _, same := range []bool{false, true} {
		r.headTags = map[string]int{}
		after := "after"
		if same {
			after = "before"
		}
		if _, err := service.UpdateDelivery(ctx, repo+":before", repo+":"+after, file); err != nil {
			t.Fatal(err)
		}
		wantAfter := 1
		if same {
			wantAfter = 0
		}
		if r.headTags["before"] != 1 || r.headTags["after"] != wantAfter {
			t.Fatalf("resolution counts: %v", r.headTags)
		}
	}
	for _, request := range requests {
		if strings.Contains(request, "/blobs/"+opaquePayload.Digest.String()) || strings.Contains(request, "/blobs/"+opaqueConfig.Digest.String()) {
			t.Fatalf("opaque read: %s", request)
		}
	}
	failure := errors.New("network denied")
	denied, err := NewService(Options{HTTPClient: &http.Client{Transport: deliveryRoundTripper(func(*http.Request) (*http.Response, error) { return nil, failure })}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := denied.UpdateDelivery(ctx, repo+":before", repo+":after", file)
	if !errors.Is(err, failure) || !reflect.DeepEqual(result, DeliveryUpdateResult{}) {
		t.Fatalf("network policy/error lost: %+v %v", result, err)
	}
}

func TestUpdateDeliveryCancellationAndAlgorithmInvalidAreEmpty(t *testing.T) {
	service, store, snapshot, _ := diffFixture(t)
	root := Root{Descriptor: snapshot.Descriptor, Store: store}
	file := deliveryfile.File{Bundle: snapshot.Descriptor.Digest}
	for _, during := range []bool{false, true} {
		canceled, cancel := context.WithCancel(ctx)
		before := root
		if during {
			before.Store = &diffGuardStore{ReadOnlyStorage: store, onFetch: cancel}
		} else {
			cancel()
		}
		result, err := service.UpdateDeliveryRoots(canceled, before, root, file)
		cancel()
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, DeliveryUpdateResult{}) {
			t.Fatalf("cancellation: %+v %v", result, err)
		}
	}
	for _, invalid := range []deliveryfile.File{{}, {Bundle: digest.FromString("stale bundle")}, {Bundle: snapshot.Descriptor.Digest, Artifacts: []deliveryfile.Artifact{{Ref: "invalid", Path: "bad"}}}} {
		result, err := service.UpdateDeliveryRoots(ctx, root, root, invalid)
		if !errors.Is(err, ErrInvalid) || !reflect.DeepEqual(result, DeliveryUpdateResult{}) {
			t.Fatalf("invalid delivery: %+v %v", result, err)
		}
	}
}

func TestUpdateDeliverySecondMetadataPassPreservesSourceErrors(t *testing.T) {
	service, store, snapshot, _ := diffFixture(t)
	probe := &deliveryFailingStore{ReadOnlyStorage: store, root: snapshot.Descriptor, failAfter: -1}
	root := Root{Descriptor: snapshot.Descriptor, Store: probe}
	if _, err := service.inspectRoot(ctx, root, bundle.InspectSnapshotGraph); err != nil {
		t.Fatal(err)
	}
	for _, cause := range []error{os.ErrPermission, context.Canceled, context.DeadlineExceeded} {
		source := &deliveryFailingStore{ReadOnlyStorage: store, root: snapshot.Descriptor, failAfter: probe.reads, cause: cause}
		result, err := service.UpdateDeliveryRoots(ctx, Root{Descriptor: snapshot.Descriptor, Store: source}, root, deliveryfile.File{Bundle: snapshot.Descriptor.Digest})
		if !errors.Is(err, cause) || errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(result, DeliveryUpdateResult{}) {
			t.Fatalf("source failure misclassified: %+v %v", result, err)
		}
	}
}

func TestUpdateDeliveryCancellationBetweenOperands(t *testing.T) {
	for _, cancelAfter := range []string{"before", "after"} {
		t.Run(cancelAfter, func(t *testing.T) {
			canceled, cancel := context.WithCancel(ctx)
			defer cancel()
			secondRead := false
			result, err := deliveryUpdateInputs(canceled, "first", "second", deliveryfile.File{}, func() (deliveryUpdateSnapshot, error) {
				if cancelAfter == "before" {
					cancel()
				}
				return deliveryUpdateSnapshot{}, nil
			}, func() (deliveryUpdateSnapshot, error) {
				secondRead = true
				cancel()
				return deliveryUpdateSnapshot{}, nil
			})
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, DeliveryUpdateResult{}) || (cancelAfter == "before" && secondRead) {
				t.Fatalf("cancellation: secondRead=%v result=%+v error=%v", secondRead, result, err)
			}
		})
	}
}

func TestUpdateDeliveryConflictRetainsDiagnosticResult(t *testing.T) {
	r, repo, service := testRegistry(t)
	a, b := object(t, r.store, "old"), object(t, r.store, "new")
	r.tags["latest"] = a
	selection := config.Selection{Ref: repo + ":latest", Referrers: policy("none"), Dependencies: dependency.Policy{Follow: "none"}}
	beforeGraph, before, err := service.capture(ctx, config.Recipe{Include: []config.Selection{selection}})
	if err != nil {
		t.Fatal(err)
	}
	r.tags["latest"] = b
	pinned := selection
	pinned.Ref = repo + "@" + a.Digest.String()
	afterGraph, after, err := service.capture(ctx, config.Recipe{Include: []config.Selection{selection, pinned}})
	if err != nil {
		t.Fatal(err)
	}
	store := memory.New()
	for _, entry := range []struct {
		graph *graph
		root  v1.Descriptor
	}{{beforeGraph, before.Descriptor}, {afterGraph, after.Descriptor}} {
		if err := copyGraph(ctx, entry.graph, store, entry.root, &Stats{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	file := deliveryfile.File{Bundle: before.Descriptor.Digest, Artifacts: []deliveryfile.Artifact{{Ref: pinned.Ref, Path: "custom/app"}}}
	result, err := service.UpdateDeliveryRoots(ctx, Root{Descriptor: before.Descriptor, Store: store}, Root{Descriptor: after.Descriptor, Store: store}, file)
	var typed *Error
	if !errors.Is(err, ErrConflict) || !errors.As(err, &typed) || result.Delivery != nil || len(result.Conflicts) != 1 {
		t.Fatalf("conflict diagnostics lost: %+v %v", result, err)
	}
	if result.Before.Digest != before.Descriptor.Digest || result.After.Digest != after.Descriptor.Digest || result.Conflicts[0].Reason != "source assignment has multiple targets" {
		t.Fatalf("conflict identities: %+v", result)
	}
}
