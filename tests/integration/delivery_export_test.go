package integration_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content/oci"
)

func TestPublicDeliveryExportsNestedInventoryAfterSourcesDisappear(t *testing.T) {
	ctx := t.Context()
	source := httptest.NewServer(newSDKRegistry())
	duplicate := httptest.NewServer(newSDKRegistry())
	holding := httptest.NewServer(newSDKRegistry())
	defer source.Close()
	defer duplicate.Close()
	defer holding.Close()
	sourceHost, duplicateHost, holdingHost := strings.TrimPrefix(source.URL, "http://"), strings.TrimPrefix(duplicate.URL, "http://"), strings.TrimPrefix(holding.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{sourceHost, duplicateHost, holdingHost}})
	if err != nil {
		t.Fatal(err)
	}
	chart := publishDependencyArtifact(t, client, sourceHost, dependencyArtifact(t, "chart", false), "chart")
	image := publishDependencyArtifact(t, client, sourceHost, dependencyArtifact(t, "image", true), "image", "current")
	same := publishDependencyArtifact(t, client, duplicateHost, dependencyArtifact(t, "image", true), "image")
	if image.Descriptor.Digest != same.Descriptor.Digest {
		t.Fatal("fixture must share bytes across distinct repositories")
	}
	declaration, err := bale.PrepareDependencyDeclaration(ctx, chart, bale.DependencyDeclaration{Dependencies: []bale.DependencyReference{{Ref: image.Repository + ":image"}, {Ref: image.Repository + ":current"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Attach(ctx, declaration); err != nil {
		t.Fatal(err)
	}
	innerRef := holdingHost + "/bundles:inner"
	inner, err := client.Create(ctx, bale.Recipe{Include: []bale.Selection{{Ref: chart.Repository + ":chart"}}}, innerRef, bale.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	outerRef := holdingHost + "/bundles:outer"
	outer, err := client.Create(ctx, bale.Recipe{Include: []bale.Selection{{Ref: innerRef}, {Ref: same.Repository + ":image"}}}, outerRef, bale.CreateOptions{ExportDelivery: true})
	if err != nil || outer.Delivery == nil {
		t.Fatalf("exact capture export: %+v %v", outer, err)
	}
	source.Close()
	duplicate.Close()
	var resolutions atomic.Int32
	reader, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{holdingHost}, HTTPClient: &http.Client{Transport: inspectRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != holdingHost || (request.Method != http.MethodGet && request.Method != http.MethodHead) || strings.Contains(request.URL.Path, "/referrers/") {
			t.Errorf("export escaped captured content: %s %s", request.Method, request.URL)
			return nil, os.ErrPermission
		}
		if request.URL.Path == "/v2/bundles/manifests/outer" {
			resolutions.Add(1)
		}
		return http.DefaultTransport.RoundTrip(request)
	})}})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := reader.ExportDelivery(ctx, outerRef)
	if err != nil || !reflect.DeepEqual(remote, *outer.Delivery) || resolutions.Load() != 1 {
		t.Fatalf("registry export differs from capture: %+v %v resolutions=%d", remote, err, resolutions.Load())
	}
	identities := map[string]bool{}
	for i, artifact := range remote.Artifacts {
		if identities[artifact.Ref] || (i > 0 && remote.Artifacts[i-1].Ref >= artifact.Ref) {
			t.Fatalf("inventory not unique/sorted: %+v", remote)
		}
		identities[artifact.Ref] = true
	}
	for _, metadata := range []bale.Metadata{inner.Metadata, outer.Metadata} {
		for _, origin := range metadata.Origins {
			if !identities[origin.Repository+"@"+origin.Descriptor.Digest.String()] {
				t.Fatalf("lost nested origin: %+v", origin)
			}
		}
	}
	if !identities[image.Repository+"@"+image.Descriptor.Digest.String()] || !identities[same.Repository+"@"+same.Descriptor.Digest.String()] {
		t.Fatal("byte deduplication erased source identities")
	}
	layout := filepath.Join(t.TempDir(), "bundle")
	if _, err := reader.Pull(ctx, outerRef, layout, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	holding.Close()
	offline, err := bale.NewClient(bale.ClientOptions{HTTPClient: &http.Client{Transport: inspectRoundTripper(func(request *http.Request) (*http.Response, error) {
		t.Errorf("offline export accessed network: %s", request.URL)
		return nil, os.ErrPermission
	})}})
	if err != nil {
		t.Fatal(err)
	}
	before := inspectLayoutFiles(t, layout)
	local, err := offline.ExportDeliveryLayout(ctx, layout)
	if err != nil || !reflect.DeepEqual(local, remote) || !reflect.DeepEqual(before, inspectLayoutFiles(t, layout)) {
		t.Fatalf("offline export changed report/layout: %+v %v", local, err)
	}
	pinned, err := offline.ExportDeliveryRoot(ctx, bale.Root{Descriptor: outer.Descriptor, Store: oci.NewStorageFromFS(os.DirFS(layout))})
	if err != nil || !reflect.DeepEqual(pinned, remote) {
		t.Fatalf("pinned export differs: %+v %v", pinned, err)
	}
	// Corruption prevents generation of a delivery configuration.
	member := inner.Metadata.Roots[0].Descriptor
	memberPath := filepath.Join(layout, "blobs", member.Digest.Algorithm().String(), member.Digest.Encoded())
	if err := os.Chmod(memberPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memberPath, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	invalid, err := offline.ExportDeliveryLayout(ctx, layout)
	if !errors.Is(err, bale.ErrIntegrity) || invalid.Bundle != "" || len(invalid.Artifacts) != 0 {
		t.Fatalf("corrupt graph yielded inventory: %+v %v", invalid, err)
	}
}
