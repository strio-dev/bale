package main

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
)

func TestPushPreviewPlacements(t *testing.T) {
	app := previewDescriptor("app")
	child := previewDescriptor("child")
	record := previewDescriptor("destination attachment")
	bundle := previewDescriptor("bundle")
	plan := bale.DeliveryPlan{
		Descriptor: bundle, Destination: "target.example/bundles/release:complete",
		Relocations: bale.Relocations{{
			Source:      bale.ArtifactID{Repository: "source.example/app", Digest: app.Digest},
			Destination: bale.ArtifactID{Repository: "target.example/custom/app", Digest: app.Digest},
			Descriptor:  app,
		}},
		Repositories: []bale.DeliveryRepository{
			{Repository: "target.example/required/index", Roots: []v1.Descriptor{child, app}},
			{Repository: "target.example/custom/app", Roots: []v1.Descriptor{app}},
		},
		Tags: []bale.Tag{
			{Repository: "target.example/custom/app", Reference: "stable", Descriptor: app},
			{Repository: "target.example/custom/app", Reference: "all", Descriptor: app},
		},
		Destinations: []bale.DestinationAttachment{{
			Repository: "target.example/custom/app", Descriptor: record, Subject: app,
		}},
	}
	before, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := writePushPreview(&output, plan); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, expected := range []string{
		"Dry run: nothing published.", "Bundle: " + bundle.Digest.String(),
		"Completion tag: target.example/bundles/release:complete",
		"destination availability, authentication and existing tags not checked",
		"complete OCI graphs", "bundle capture is also copied", "do not remove copies required",
		"Canonical relocations (1)", "source.example/app@" + app.Digest.String(),
		"target.example/custom/app@" + app.Digest.String(),
		"Graph copy roots (3)", "target.example/required/index@" + app.Digest.String(),
		"target.example/required/index@" + child.Digest.String(),
		"Restored tags (2)", "target.example/custom/app:stable", "target.example/custom/app:all",
		"Generated destination attachments (1)", "target.example/custom/app@" + record.Digest.String(),
		"ATTACHMENT", "SUBJECT",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("missing %q:\n%s", expected, text)
		}
	}
	if strings.Count(text, plan.Destination) != 1 {
		t.Fatalf("completion tag must be separate from restored tags:\n%s", text)
	}
	if strings.Contains(text, "bytes") || strings.Contains(text, "dedupe") {
		t.Fatalf("copy roots must not imply payload accounting:\n%s", text)
	}
	after, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("preview mutated the caller's plan")
	}
	slices.Reverse(plan.Repositories)
	for i := range plan.Repositories {
		slices.Reverse(plan.Repositories[i].Roots)
	}
	slices.Reverse(plan.Tags)
	var reordered bytes.Buffer
	if err := writePushPreview(&reordered, plan); err != nil {
		t.Fatal(err)
	}
	if reordered.String() != text {
		t.Fatalf("plan order changed report:\n%s", reordered.String())
	}
}

func TestPushPreviewEmpty(t *testing.T) {
	var output bytes.Buffer
	if err := writePushPreview(&output, bale.DeliveryPlan{Descriptor: previewDescriptor("empty"), Destination: "target.example/bundle:complete"}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Canonical relocations (0)", "Graph copy roots (0)", "Restored tags (0)", "Generated destination attachments (0)"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing %q:\n%s", expected, output.String())
		}
	}
}

func TestPushPreviewPreparationFailureWritesNothing(t *testing.T) {
	plan := bale.DeliveryPlan{Tags: []bale.Tag{{Repository: "target.example/app", Reference: "bad\nrow", Descriptor: previewDescriptor("app")}}}
	var output bytes.Buffer
	if err := writePushPreview(&output, plan); err == nil || !strings.Contains(err.Error(), "push preview") {
		t.Fatalf("expected push preview error, got %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("partial preview emitted: %q", output.String())
	}
}

func TestPushPreviewWriterFailure(t *testing.T) {
	plan := bale.DeliveryPlan{Descriptor: previewDescriptor("bundle"), Destination: "target.example/bundle:complete"}
	if err := writePushPreview(brokenWriter{}, plan); err == nil {
		t.Fatal("writer failure was lost")
	}
	if err := writePushPreview(io.Discard, plan); err != nil {
		t.Fatal(err)
	}
}
