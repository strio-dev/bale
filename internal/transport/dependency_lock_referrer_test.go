package transport

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
)

func TestDependencyBundleOuterReferrersKeepMemberLock(t *testing.T) {
	r, repo, service := testRegistry(t)
	member := object(t, r.store, "inner root")
	r.tags["member"] = member
	_, captured, err := service.capture(ctx, config.Recipe{Include: []config.Selection{dependencySelection(repo + ":member")}})
	if err != nil {
		t.Fatal(err)
	}
	inner, err := bundle.BuildSnapshot(ctx, r.store, captured.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	r.tags["bundle"] = inner.Descriptor
	// Newly declared member dependencies must not execute through the lock.
	dependencyDeclaration(t, r, member, "", dependency.Reference{Ref: repo + ":must-not-resolve"})
	outerAttachment := refIndex(t, r, inner.Descriptor, []v1.Descriptor{})
	external := object(t, r.store, "outer attachment dependency")
	r.tags["external"] = external
	// Also exercise a cycle back to the locked bundle through its outer referrer.
	dependencyDeclaration(t, r, outerAttachment, "", dependency.Reference{Ref: repo + ":external"}, dependency.Reference{Ref: repo + ":bundle"})
	input := dependencySelection(repo + ":bundle")
	input.Referrers = policy("all")
	disabled := input
	disabled.Dependencies.Follow = "none"
	_, optedOut, err := service.capture(ctx, config.Recipe{Include: []config.Selection{disabled}})
	if err != nil {
		t.Fatal(err)
	}
	if capturedOrigin(optedOut.Metadata, external) {
		t.Fatal("opted-out bundle expanded its outer attachments")
	}
	_, outer, err := service.capture(ctx, config.Recipe{Include: []config.Selection{disabled, input}})
	if err != nil {
		t.Fatal(err)
	}
	if !capturedOrigin(outer.Metadata, external) {
		t.Fatal("new outer referrer's dependency was omitted")
	}
	if r.headTags["must-not-resolve"] != 0 {
		t.Fatal("locked member selector was re-executed")
	}
}

func TestDependencyDiscoveryBoundsCandidateReads(t *testing.T) {
	for _, mode := range []string{"advertised type", "untyped", "head size"} {
		t.Run(mode, func(t *testing.T) {
			r, _, _ := testRegistry(t)
			root := object(t, r.store, "subject")
			r.tags["root"] = root
			candidate := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString("oversized candidate"), Size: dependency.MaxBytes + 1, ArtifactType: dependency.ArtifactType}
			if mode == "untyped" {
				candidate.ArtifactType = ""
			}
			if mode == "head size" {
				candidate.Size = 1
			}
			r.refs[root.Digest.String()] = []v1.Descriptor{candidate}
			var bodyReads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if strings.HasSuffix(request.URL.Path, candidate.Digest.String()) {
					if request.Method == http.MethodGet {
						bodyReads.Add(1)
					}
					w.Header().Set("Content-Type", v1.MediaTypeImageManifest)
					w.Header().Set("Content-Length", fmt.Sprint(dependency.MaxBytes+1))
					w.Header().Set("Docker-Content-Digest", candidate.Digest.String())
					w.WriteHeader(http.StatusOK)
					return
				}
				r.serve(w, request)
			}))
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "http://")
			service, err := NewService(Options{PlainHTTPRegistries: []string{host}})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = service.capture(ctx, config.Recipe{Include: []config.Selection{dependencySelection(host + "/test/artifacts:root")}})
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("expected size rejection, got %v", err)
			}
			if bodyReads.Load() != 0 {
				t.Fatal("oversized candidate body downloaded before size rejection")
			}
		})
	}
}
