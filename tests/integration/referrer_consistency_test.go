package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
)

func TestRegistryDiscoveryRejectsMalformedInventoryAcrossOperations(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			ctx := context.Background()
			registry := newSDKRegistry()
			var malformed atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if native && strings.Contains(r.URL.Path, "/referrers/") {
					w.Header().Set("Content-Type", v1.MediaTypeImageIndex)
					if malformed.Load() {
						_, _ = io.WriteString(w, `{"schemaVersion":3,"manifests":[]}`)
					} else {
						_, _ = io.WriteString(w, `{"schemaVersion":2,"manifests":[]}`)
					}
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
			recipe := bale.Recipe{Include: []bale.Selection{{Ref: subject.Repository + ":a"}}}
			bundleRef := host + "/bundles:complete"
			if _, err := client.Create(ctx, recipe, bundleRef, bale.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			malformed.Store(true)
			if !native {
				body := []byte(`{"schemaVersion":3,"manifests":[]}`)
				key := digest.FromBytes(body).String()
				registry.mu.Lock()
				registry.objects[key], registry.types[key] = body, v1.MediaTypeImageIndex
				registry.tags[strings.Replace(subject.Descriptor.Digest.String(), ":", "-", 1)] = key
				registry.mu.Unlock()
			}
			for _, op := range []string{"plan", "create", "latest pull"} {
				t.Run(op, func(t *testing.T) {
					var err error
					registry.mu.Lock()
					before := registry.writes
					registry.mu.Unlock()
					switch op {
					case "plan":
						_, err = client.Plan(ctx, bale.TransferRequest{Roots: []bale.Root{subject}, Destination: host + "/mirror"})
					case "create":
						_, err = client.Create(ctx, recipe, host+"/bundles:new", bale.CreateOptions{})
					case "latest pull":
						_, err = client.Pull(ctx, bundleRef, filepath.Join(t.TempDir(), "latest"), bale.PullLatest)
					}
					if !errors.Is(err, bale.ErrIntegrity) {
						t.Fatalf("%s accepted malformed inventory: %v", op, err)
					}
					registry.mu.Lock()
					after := registry.writes
					registry.mu.Unlock()
					if after != before {
						t.Fatalf("%s wrote before discovery failed", op)
					}
				})
			}
			if _, err := client.Pull(ctx, bundleRef, filepath.Join(t.TempDir(), "locked"), bale.PullLocked); err != nil {
				t.Fatalf("locked pull consulted live inventory: %v", err)
			}
		})
	}
}

func TestTransferConfirmationRejectsMalformedInventory(t *testing.T) {
	for _, operation := range []string{"execute", "push"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			registry := newSDKRegistry()
			var malformed atomic.Bool
			var confirming atomic.Bool
			var advertised v1.Descriptor
			var managedTag string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Corrupt the inventory after the selected attachment's re-push,
				// so preflight succeeds and confirmation must reject the response.
				if malformed.Load() && r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/manifests/"+advertised.Digest.String()) {
					confirming.Store(true)
				}
				if malformed.Load() && confirming.Load() && r.Method == http.MethodGet && (strings.Contains(r.URL.Path, "/referrers/") || strings.HasSuffix(r.URL.Path, "/manifests/"+managedTag)) {
					w.Header().Set("Content-Type", v1.MediaTypeImageIndex)
					_ = json.NewEncoder(w).Encode(v1.Index{Versioned: specs.Versioned{SchemaVersion: 3}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{advertised}})
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
			attachment := attachmentFixture(t, &subject.Descriptor, true, "attachment")
			attachment.Subject = subject.Repository + ":a"
			if _, err := client.Attach(ctx, attachment); err != nil {
				t.Fatal(err)
			}
			advertised = attachment.Descriptor
			managedTag = strings.Replace(subject.Descriptor.Digest.String(), ":", "-", 1)
			executePlan, err := client.Plan(ctx, bale.TransferRequest{Roots: []bale.Root{subject}, Destination: host + "/mirror"})
			if err != nil {
				t.Fatal(err)
			}
			recipe := bale.Recipe{Include: []bale.Selection{{Ref: subject.Repository + ":a"}}}
			bundleRef := host + "/bundles:complete"
			if _, err := client.Create(ctx, recipe, bundleRef, bale.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			layout := filepath.Join(t.TempDir(), "layout")
			if _, err := client.Pull(ctx, bundleRef, layout, bale.PullLocked); err != nil {
				t.Fatal(err)
			}
			pushPlan, err := client.PlanPush(ctx, layout, host+"/delivery:complete", bale.PushOptions{})
			if err != nil {
				t.Fatal(err)
			}
			malformed.Store(true)
			if operation == "execute" {
				_, err = client.Execute(ctx, executePlan)
			} else {
				_, err = client.Push(ctx, pushPlan)
			}
			if !errors.Is(err, bale.ErrIntegrity) {
				t.Fatalf("%s confirmed malformed inventory: %v", operation, err)
			}
			malformed.Store(false)
			if operation == "execute" {
				_, err = client.Execute(ctx, executePlan)
			} else {
				_, err = client.Push(ctx, pushPlan)
			}
			if err != nil {
				t.Fatalf("%s retry after inventory correction: %v", operation, err)
			}
		})
	}
}
