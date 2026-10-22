package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/strio-dev/bale"
)

// This registry scopes objects and tags to repositories so successful global
// digest lookup cannot hide missing structural or subject-side copies.
type deliveryRegistry struct {
	mu           sync.Mutex
	repositories map[string]*sdkRegistry
}

func (r *deliveryRegistry) repository(name string) *sdkRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.repositories == nil {
		r.repositories = map[string]*sdkRegistry{}
	}
	if r.repositories[name] == nil {
		r.repositories[name] = newSDKRegistry()
	}
	return r.repositories[name]
}
func (r *deliveryRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, "/v2/")
	for _, marker := range []string{"/blobs/", "/manifests/", "/referrers/"} {
		if repository, _, ok := strings.Cut(path, marker); ok {
			r.repository(repository).ServeHTTP(w, req)
			return
		}
	}
	http.NotFound(w, req)
}

func TestDeliveryArtifactPlacementScopedRegistryAndRetry(t *testing.T) {
	ctx := context.Background()
	sourceRegistry := &deliveryRegistry{}
	sourceServer := httptest.NewServer(sourceRegistry)
	defer sourceServer.Close()
	sourceHost := strings.TrimPrefix(sourceServer.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{sourceHost}})
	if err != nil {
		t.Fatal(err)
	}
	subject := attachmentSubject(t, client, sourceHost)
	attachment := attachmentFixture(t, &subject.Descriptor, false, "delivery attachment")
	attachment.Subject = subject.Repository + ":a"
	if _, err = client.Attach(ctx, attachment); err != nil {
		t.Fatal(err)
	}
	bundleRef := sourceHost + "/bundles:captured"
	created, err := client.Create(ctx, bale.Recipe{Include: []bale.Selection{{Ref: subject.Repository + ":a"}}}, bundleRef, bale.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(t.TempDir(), "layout")
	if _, err = client.Pull(ctx, bundleRef, layout, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	targetRegistry := &deliveryRegistry{}
	targetServer := httptest.NewServer(targetRegistry)
	defer targetServer.Close()
	targetHost := strings.TrimPrefix(targetServer.URL, "http://")
	target, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{targetHost}})
	if err != nil {
		t.Fatal(err)
	}
	file := bale.DeliveryFile{Bundle: created.Descriptor.Digest, Artifacts: []bale.DeliveryArtifact{
		{Ref: subject.Repository + "@" + subject.Descriptor.Digest.String(), Path: "application"},
		{Ref: subject.Repository + "@" + attachment.Descriptor.Digest.String(), Path: "attachments"},
	}}
	plan, err := target.PlanPush(ctx, layout, targetHost+"/release:complete", bale.PushOptions{Delivery: &file})
	if err != nil {
		t.Fatal(err)
	}
	targetRegistry.repository("release/application").failAlias = "a"
	partial, err := target.Push(ctx, plan)
	if err == nil {
		t.Fatal("injected alias failure did not fail push")
	}
	if len(partial.Tags) != 0 || len(partial.Relocations) != 2 {
		t.Fatalf("untruthful partial report: %+v", partial)
	}
	for _, relocation := range partial.Relocations {
		expected := "application"
		if relocation.Source.Digest == attachment.Descriptor.Digest {
			expected = "attachments"
		}
		if relocation.Destination.Repository != targetHost+"/release/"+expected {
			t.Fatalf("incidental copy reported as canonical: %+v", relocation)
		}
	}
	targetRegistry.repository("release/application").failAlias = ""
	result, err := target.Push(ctx, plan)
	if err != nil || len(result.Relocations) != 2 || len(result.Tags) != 1 {
		t.Fatalf("retry result %+v err=%v", result, err)
	}
	for _, repository := range []string{"release/application", "release/attachments", "release"} {
		resolved, err := target.Resolve(ctx, targetHost+"/"+repository+"@"+subject.Descriptor.Digest.String())
		if err != nil || resolved.Descriptor.Digest != subject.Descriptor.Digest {
			t.Fatalf("subject missing in %s: %v", repository, err)
		}
		refs, err := target.ListReferrers(ctx, targetHost+"/"+repository+"@"+subject.Descriptor.Digest.String(), bale.ListReferrersOptions{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, descriptor := range refs {
			if descriptor.Digest == attachment.Descriptor.Digest {
				found = true
			}
		}
		if !found {
			t.Fatalf("attachment not discoverable in %s: %+v", repository, refs)
		}
	}
}
