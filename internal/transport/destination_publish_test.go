package transport

import (
	"errors"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
)

func TestDestinationPlanningPreservesScopesContextsAndDirectEdges(t *testing.T) {
	r, repo, service := testRegistry(t)
	a, b, c := object(t, r.store, "a"), object(t, r.store, "b"), object(t, r.store, "c")
	r.tags["a"], r.tags["b"], r.tags["c"] = a, b, c
	da := dependencyDeclaration(t, r, a, "production", dependency.Reference{Ref: repo + ":b"})
	db := dependencyDeclaration(t, r, b, "", dependency.Reference{Ref: repo + ":c"})
	_, snap, err := service.capture(ctx, config.Recipe{Include: []config.Selection{dependencySelection(repo + ":a")}})
	if err != nil {
		t.Fatal(err)
	}
	// Two independently captured snapshots of the same subject retain distinct
	// observations, rather than merging their meaning under one policy.
	other := bundle.Snapshot{Descriptor: v1.Descriptor{Digest: digest.FromString("other capture")}, Metadata: bundle.Metadata{Dependencies: []bundle.DependencyResolution{{Repository: repo, Subject: a, Policy: dependency.Policy{Follow: "none"}, Outcome: "disabled", Dependencies: []bundle.ResolvedDependency{}}}}}
	plan := DeliveryPlan{Destination: "target.example/release:complete", Descriptor: v1.Descriptor{Digest: digest.FromString("completion")}}
	for _, d := range []v1.Descriptor{a, b, c, da, db} {
		plan.Relocations = append(plan.Relocations, Relocation{Source: ArtifactID{Repository: repo, Digest: d.Digest}, Destination: ArtifactID{Repository: "target.example/release/member", Digest: d.Digest}, Descriptor: d})
	}
	attachments, err := planDestinationAttachments(ctx, r.store, []bundle.Snapshot{snap, other}, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 3 {
		t.Fatalf("want ordinary artifacts only: %+v", attachments)
	}
	for _, attachment := range attachments {
		if attachment.Subject.Digest == a.Digest {
			if len(attachment.Record.Resolutions) != 2 {
				t.Fatalf("lost scopes: %+v", attachment.Record)
			}
			var selected, disabled bool
			for _, resolution := range attachment.Record.Resolutions {
				switch resolution.Outcome {
				case "selected":
					selected = true
					if resolution.Capture != snap.Descriptor.Digest || resolution.Declaration == nil || resolution.Declaration.Context != "production" || len(resolution.Dependencies) != 1 || resolution.Dependencies[0].Ref != repo+":b" {
						t.Fatalf("lost direct/context semantics: %+v", resolution)
					}
				case "disabled":
					disabled = true
					if resolution.Capture != other.Descriptor.Digest {
						t.Fatal("lost disabled capture")
					}
				}
			}
			if !selected || !disabled {
				t.Fatal("lost outcomes")
			}
		}
		if err := confirmDestinationMappings(attachment.Record, plan.Relocations); err != nil {
			t.Fatal(err)
		}
		if err := confirmDestinationMappings(attachment.Record, nil); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("accepted unconfirmed subject: %v", err)
		}
	}
	stores, err := prepareDestinations(ctx, r.store, attachments)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range attachments {
		if err := bundle.VerifyClosure(ctx, stores, a.Descriptor); err != nil {
			t.Fatal(err)
		}
	}
	attachments[0].Descriptor.Digest = digest.FromString("tampered")
	if _, err := prepareDestinations(ctx, r.store, attachments); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("accepted changed prepared attachment: %v", err)
	}
}
