package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func TestCreateNestedDeliveryUsesCapturedInventoryWithoutOpaqueReads(t *testing.T) {
	registry, repository, service := testRegistry(t)
	configDescriptor := put(t, registry.store, "application/example.config", []byte("application config"))
	layer := put(t, registry.store, "application/example.payload", []byte("application payload"))
	manifestBytes, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: configDescriptor, Layers: []v1.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	artifact := put(t, registry.store, v1.MediaTypeImageManifest, manifestBytes)
	registry.tags["stable"] = artifact
	_, capture, err := service.capture(ctx, config.Recipe{Include: []config.Selection{{Ref: repository + ":stable", Referrers: policy("none"), Dependencies: config.Selection{}.Dependencies}}})
	if err != nil {
		t.Fatal(err)
	}
	inner, err := bundle.BuildSnapshot(ctx, registry.store, capture.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	registry.tags["inner"] = inner.Descriptor
	var manifest v1.Manifest
	raw, err := content.FetchAll(ctx, registry.store, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	opaque := map[string]bool{}
	for _, descriptor := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
		opaque[descriptor.Digest.String()] = true
	}
	opaqueReads := 0
	service, err = NewService(Options{PlainHTTPRegistries: []string{strings.Split(repository, "/")[0]}, HTTPClient: &http.Client{Transport: deliveryRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodGet {
			_, reference, _ := strings.Cut(request.URL.Path, "/blobs/")
			if opaque[reference] {
				opaqueReads++
			}
		}
		return http.DefaultTransport.RoundTrip(request)
	})}})
	if err != nil {
		t.Fatal(err)
	}
	selection := config.Selection{Ref: repository + ":inner", Referrers: policy("none")}
	created, err := service.Create(ctx, config.Recipe{Include: []config.Selection{selection}}, repository+":outer", CreateOptions{DryRun: true, ExportDelivery: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.Delivery == nil || created.Delivery.Bundle != created.Descriptor.Digest || registry.headTags["inner"] != 1 {
		t.Fatalf("export did not use exact capture: %+v resolutions=%d", created, registry.headTags["inner"])
	}
	// Interpreted Bale metadata may be fetched. Application config/layers must
	// remain HEAD-checked pinned sources, even inside an existing bundle.
	for _, entry := range created.Delivery.Artifacts {
		for _, opaque := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
			if entry.Ref == repository+"@"+opaque.Digest.String() {
				t.Fatalf("opaque payload exported as artifact: %+v", entry)
			}
		}
	}
	if opaqueReads != 0 {
		t.Fatalf("export downloaded application payloads %d times", opaqueReads)
	}
	without, err := service.Create(ctx, config.Recipe{Include: []config.Selection{selection}}, repository+":outer", CreateOptions{DryRun: true})
	if err != nil || without.Descriptor.Digest != created.Descriptor.Digest || !reflect.DeepEqual(without.Metadata, created.Metadata) {
		t.Fatalf("delivery export changed bundle: %+v %v", without, err)
	}
}

type deliveryRoundTripper func(*http.Request) (*http.Response, error)

func (f deliveryRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestExportDeliveryPreservesSecondPassSourceErrors(t *testing.T) {
	store := memory.New()
	artifact := object(t, store, "payload")
	snapshot := snapshotFor(t, store, []string{"source.example/app"}, []v1.Descriptor{artifact})
	probe := &deliveryFailingStore{ReadOnlyStorage: store, root: snapshot.Descriptor, failAfter: -1}
	service := &Service{}
	if _, err := service.InspectRoot(ctx, Root{Descriptor: snapshot.Descriptor, Store: probe}); err != nil {
		t.Fatal(err)
	}
	for _, cause := range []error{os.ErrPermission, context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			source := &deliveryFailingStore{ReadOnlyStorage: store, root: snapshot.Descriptor, failAfter: probe.reads, cause: cause}
			file, err := service.ExportDeliveryRoot(ctx, Root{Descriptor: snapshot.Descriptor, Store: source})
			if !errors.Is(err, cause) || errors.Is(err, ErrIntegrity) || file.Bundle != "" {
				t.Fatalf("source failure misclassified or exported: %+v %v", file, err)
			}
		})
	}
}

type deliveryFailingStore struct {
	content.ReadOnlyStorage
	root             v1.Descriptor
	reads, failAfter int
	cause            error
}

func (s *deliveryFailingStore) Fetch(ctx context.Context, descriptor v1.Descriptor) (io.ReadCloser, error) {
	if descriptor.Digest == s.root.Digest {
		s.reads++
		if s.failAfter >= 0 && s.reads > s.failAfter {
			return nil, s.cause
		}
	}
	return s.ReadOnlyStorage.Fetch(ctx, descriptor)
}
