package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
)

// Caller providers retain their exact ORAS capability, transport and inventory
// semantics. Bale still validates the bytes and subject of selected envelopes.
func TestPlanPreservesCallerOwnedRemoteReferrerSource(t *testing.T) {
	for _, requiredOnly := range []bool{false, true} {
		for _, capable := range []bool{false, true} {
			t.Run(fmt.Sprintf("required=%v/capable=%v", requiredOnly, capable), func(t *testing.T) {
				r, destination, s := testRegistry(t)
				subject := object(t, r.store, "subject")
				attachment := refIndex(t, r, subject, []v1.Descriptor{})
				raw, _ := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{attachment}})
				inventory := put(t, r.store, v1.MediaTypeImageIndex, raw)
				fallbackTag := strings.Replace(subject.Digest.String(), ":", "-", 1)
				r.tags[fallbackTag] = inventory
				var nativeRequests, fallbackRequests atomic.Int64
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.Header.Get("Authorization") != "Bearer caller-owned" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					if strings.Contains(req.URL.Path, "/referrers/") {
						nativeRequests.Add(1)
						if req.URL.Query().Get("n") != "7" {
							t.Errorf("caller pagination lost: %s", req.URL.RawQuery)
						}
						if capable {
							w.WriteHeader(http.StatusNotFound)
						} else {
							w.WriteHeader(http.StatusForbidden)
						}
						return
					}
					if strings.HasSuffix(req.URL.Path, "/manifests/"+fallbackTag) {
						fallbackRequests.Add(1)
					}
					r.serve(w, req)
				}))
				defer server.Close()
				repo, err := remote.NewRepository(strings.TrimPrefix(server.URL, "https://") + "/caller/artifacts")
				if err != nil {
					t.Fatal(err)
				}
				repo.Client = bearerClient{server.Client()}
				repo.ReferrerListPageSize = 7
				repo.ReferrerListMaxPages = 2
				if err := repo.SetReferrersCapability(capable); err != nil {
					t.Fatal(err)
				}
				root := Root{Repository: "provenance.invalid/source", Descriptor: subject, Store: readOnlySource{r.store}, ReferrerSource: repo, Referrers: policy("all")}
				if requiredOnly {
					root.Referrers = policy("none", attachment)
				}
				plan, err := s.Plan(context.Background(), TransferRequest{Roots: []Root{root}, Destination: destination})
				if capable {
					if err == nil {
						t.Fatal("forced native failure was hidden by fallback")
					}
					if nativeRequests.Load() != 1 || fallbackRequests.Load() != 0 {
						t.Fatalf("lost forced native semantics: native=%d fallback=%d", nativeRequests.Load(), fallbackRequests.Load())
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if len(plan.View().Referrers) != 1 {
						t.Fatalf("lost attachment: %+v", plan.View())
					}
					if nativeRequests.Load() != 0 || fallbackRequests.Load() == 0 {
						t.Fatalf("lost forced fallback semantics: native=%d fallback=%d", nativeRequests.Load(), fallbackRequests.Load())
					}
				}
			})
		}
	}
}

func TestPlanCustomReferrerProviderStillValidatesSelectedSubject(t *testing.T) {
	r, destination, s := testRegistry(t)
	subject := object(t, r.store, "subject")
	other := object(t, r.store, "other")
	wrongSubject := refIndex(t, r, other, []v1.Descriptor{})
	provider := &fixedReferrers{refs: map[digest.Digest][]v1.Descriptor{subject.Digest: {wrongSubject}}, reads: map[digest.Digest]int{}}
	root := Root{Repository: "provenance.invalid/source", Descriptor: subject, Store: readOnlySource{r.store}, ReferrerSource: provider, Referrers: policy("all")}
	if _, err := s.Plan(context.Background(), TransferRequest{Roots: []Root{root}, Destination: destination}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("accepted custom provider wrong-subject attachment: %v", err)
	}
}

type bearerClient struct{ *http.Client }

func (c bearerClient) Do(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer caller-owned")
	return c.Client.Do(clone)
}

type readOnlySource struct{ content.ReadOnlyStorage }
