package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
)

type metadataExportFixture struct {
	registry    *sdkRegistry
	client      *bale.Client
	host        string
	reference   string
	created     bale.CreateResult
	opaque      map[string]bool
	interpreted map[string]bool
	nestedJSON  string
}

func newMetadataExportFixture(t *testing.T) metadataExportFixture {
	t.Helper()
	source := httptest.NewServer(newSDKRegistry())
	t.Cleanup(source.Close)
	registry := newSDKRegistry()
	holding := httptest.NewServer(registry)
	t.Cleanup(holding.Close)
	sourceHost := strings.TrimPrefix(source.URL, "http://")
	host := strings.TrimPrefix(holding.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{sourceHost, host}})
	if err != nil {
		t.Fatal(err)
	}
	chart := publishDependencyArtifact(t, client, sourceHost, dependencyArtifact(t, "metadata-export-chart", false), "chart")
	image := publishDependencyArtifact(t, client, sourceHost, dependencyArtifact(t, "metadata-export-image", true), "image", "current")
	declaration, err := bale.PrepareDependencyDeclaration(t.Context(), chart, bale.DependencyDeclaration{Dependencies: []bale.DependencyReference{{Ref: image.Repository + ":image"}, {Ref: image.Repository + ":current"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Attach(t.Context(), declaration); err != nil {
		t.Fatal(err)
	}
	inner, err := client.Create(t.Context(), bale.Recipe{Include: []bale.Selection{{Ref: chart.Repository + ":chart"}}}, host+"/bundles:inner", bale.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reference := host + "/bundles:outer"
	created, err := client.Create(t.Context(), bale.Recipe{Include: []bale.Selection{{Ref: host + "/bundles:inner"}}}, reference, bale.CreateOptions{ExportDelivery: true})
	if err != nil || created.Delivery == nil {
		t.Fatalf("create captured delivery: %+v %v", created, err)
	}
	source.Close()
	f := metadataExportFixture{registry: registry, client: client, host: host, reference: reference, created: created, opaque: map[string]bool{}, interpreted: map[string]bool{}}
	for _, value := range []string{"opaque component config", "metadata-export-chart", "metadata-export-image"} {
		f.opaque[digest.FromString(value).String()] = true
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	var innerIndex v1.Index
	if err := json.Unmarshal(registry.objects[inner.Descriptor.Digest.String()], &innerIndex); err != nil {
		t.Fatal(err)
	}
	for key, raw := range registry.objects {
		var manifest v1.Manifest
		if json.Unmarshal(raw, &manifest) == nil && (manifest.ArtifactType == bale.MetadataArtifactType || manifest.ArtifactType == bale.DependencyArtifactType) {
			for _, layer := range manifest.Layers {
				f.interpreted[layer.Digest.String()] = true
				if key == innerIndex.Annotations[bale.MetadataDigestKey] {
					f.nestedJSON = layer.Digest.String()
				}
			}
		}
	}
	if f.nestedJSON == "" || len(f.interpreted) < 3 {
		t.Fatalf("fixture lacks nested metadata/declaration JSON: %+v", f.interpreted)
	}
	return f
}

func TestPublicDeliveryMetadataExportSkipsOpaqueReads(t *testing.T) {
	f := newMetadataExportFixture(t)
	var mu sync.Mutex
	reads := map[string]int{}
	forbidden, resolutions := 0, 0
	reader, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{f.host}, HTTPClient: &http.Client{Transport: inspectRoundTripper(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		key := request.URL.Path[strings.LastIndex(request.URL.Path, "/")+1:]
		if request.URL.Host != f.host || !strings.HasPrefix(request.URL.Path, "/v2/bundles/") || (request.Method != http.MethodGet && request.Method != http.MethodHead) || strings.Contains(request.URL.Path, "/referrers/") || f.opaque[key] {
			forbidden++
			return nil, os.ErrPermission
		}
		if request.URL.Path == "/v2/bundles/manifests/outer" {
			resolutions++
		}
		if request.Method == http.MethodGet {
			reads[key]++
		}
		return http.DefaultTransport.RoundTrip(request)
	})}})
	if err != nil {
		t.Fatal(err)
	}
	f.registry.mu.Lock()
	writes := f.registry.writes
	f.registry.mu.Unlock()
	got, err := reader.ExportDelivery(t.Context(), f.reference)
	if err != nil {
		t.Fatalf("metadata export: %v (forbidden requests=%d)", err, forbidden)
	}
	if forbidden != 0 || resolutions != 1 {
		t.Fatalf("export request boundary: forbidden=%d bundle resolutions=%d", forbidden, resolutions)
	}
	for key := range f.interpreted {
		if reads[key] == 0 {
			t.Errorf("interpreted metadata/declaration JSON was not read: %s", key)
		}
	}
	assertMetadataExportDelivery(t, got, *f.created.Delivery)
	f.registry.mu.Lock()
	defer f.registry.mu.Unlock()
	if f.registry.writes != writes {
		t.Fatal("export wrote registry content")
	}
}

func TestPublicDeliveryMetadataExportLeavesPayloadValidationToFullReads(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			f := newMetadataExportFixture(t)
			payload := digest.FromString("metadata-export-image").String()
			f.registry.mu.Lock()
			if mode == "missing" {
				delete(f.registry.objects, payload)
			} else {
				f.registry.corruptRead = payload
			}
			f.registry.mu.Unlock()
			got, err := f.client.ExportDelivery(t.Context(), f.reference)
			if err != nil {
				t.Fatalf("opaque %s payload blocked metadata export: %v", mode, err)
			}
			assertMetadataExportDelivery(t, got, *f.created.Delivery)
			want := bale.ErrIntegrity
			if mode == "missing" {
				want = bale.ErrNotFound
			}
			if _, err := f.client.Inspect(t.Context(), f.reference); !errors.Is(err, want) {
				t.Fatalf("full inspection did not reject %s payload with %v: %v", mode, want, err)
			}
			if _, err := f.client.Pull(t.Context(), f.reference, filepath.Join(t.TempDir(), "bundle"), bale.PullLocked); !errors.Is(err, want) {
				t.Fatalf("locked transfer did not reject %s payload with %v: %v", mode, want, err)
			}
		})
	}
}

func TestPublicDeliveryMetadataExportRejectsCorruptNestedMetadata(t *testing.T) {
	f := newMetadataExportFixture(t)
	f.registry.mu.Lock()
	f.registry.corruptRead = f.nestedJSON
	f.registry.mu.Unlock()
	got, err := f.client.ExportDelivery(t.Context(), f.reference)
	if !errors.Is(err, bale.ErrIntegrity) || got.Bundle != "" || len(got.Artifacts) != 0 {
		t.Fatalf("corrupt nested metadata yielded delivery: %+v %v", got, err)
	}
}

func TestPublicDeliveryMetadataExportPreservesSourceErrors(t *testing.T) {
	for _, mode := range []string{"denied", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := newMetadataExportFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.registry.mu.Lock()
			f.registry.failRead = f.nestedJSON
			f.registry.mu.Unlock()
			if mode == "canceled" {
				cancel()
			}
			got, err := f.client.ExportDelivery(ctx, f.reference)
			var detail *bale.Error
			if err == nil || !errors.As(err, &detail) || errors.Is(err, bale.ErrIntegrity) || got.Bundle != "" || len(got.Artifacts) != 0 {
				t.Fatalf("source error lost classification: %+v %v", got, err)
			}
			if mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation cause lost: %v", err)
			}
		})
	}
}

func assertMetadataExportDelivery(t *testing.T, got, want bale.DeliveryFile) {
	t.Helper()
	gotYAML, err := bale.EncodeDeliveryFile(got)
	if err != nil {
		t.Fatal(err)
	}
	wantYAML, err := bale.EncodeDeliveryFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotYAML, wantYAML) {
		t.Fatalf("delivery YAML changed:\ngot:\n%swant:\n%s", gotYAML, wantYAML)
	}
	for i := 1; i < len(got.Artifacts); i++ {
		if got.Artifacts[i-1].Ref >= got.Artifacts[i].Ref {
			t.Fatalf("delivery inventory not unique and sorted: %+v", got.Artifacts)
		}
	}
}
