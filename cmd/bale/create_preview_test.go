package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
)

func previewDescriptor(value string) v1.Descriptor {
	return v1.Descriptor{Digest: digest.FromString(value), MediaType: v1.MediaTypeImageManifest, Size: 100}
}

func TestCreatePreviewInventory(t *testing.T) {
	root := previewDescriptor("root")
	child := previewDescriptor("platform child")
	attachment := previewDescriptor("attachment")
	repository := "registry.example:5000/team/artifact"
	metadata := bale.Metadata{
		Recipe: bale.Recipe{Include: []bale.Selection{{As: "production"}, {As: "development"}}},
		Roots: []bale.BundleRoot{
			{Input: 1, Repository: repository, Reference: "development", Descriptor: root},
			{Input: 0, Repository: repository, Reference: "stable", Descriptor: root},
			{Input: 0, Repository: repository, Reference: root.Digest.String(), Descriptor: root},
		},
		Origins: []bale.Origin{
			{Repository: repository, Descriptor: child},
			{Repository: repository, Descriptor: root},
			{Repository: repository, Descriptor: attachment},
		},
		Dependencies: []bale.DependencyResolution{{
			Repository: repository, Subject: root, Declaration: &attachment,
			Dependencies: []bale.ResolvedDependency{
				{Ref: repository + ":stable", Descriptor: root},
				{Ref: repository + ":development", Descriptor: root},
			},
		}},
		Plugins: []bale.PluginResolution{
			{Input: 1, Repository: repository, Subject: root, Ref: repository + ":stable", Descriptor: root, Declaration: attachment},
			{Input: 0, Repository: repository, Subject: root, Ref: repository + ":stable", Descriptor: root, Declaration: attachment},
		},
		Referrers: []bale.Association{{Repository: repository, Subject: root, Descriptor: attachment}},
	}
	result := bale.CreateResult{Descriptor: previewDescriptor("bundle"), Metadata: metadata, DryRun: true}
	var output bytes.Buffer
	if err := writeCreatePreview(&output, result); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, expected := range []string{
		"Dry run: nothing published.", "Bundle: " + result.Descriptor.Digest.String(),
		"Artifacts: 3 unique repository/digest identities",
		"Roles (may overlap): 1 input, 1 dependency, 1 plugin, 1 attachment, 1 content",
		`include[0] (as "production")`, `include[1] (as "development")`,
		repository + ":stable", repository + ":development", repository + "@" + root.Digest.String(),
		"dependency; input; plugin", repository + "@" + child.Digest.String(),
		"OCI graph",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("preview missing %q:\n%s", expected, text)
		}
	}
	if strings.Count(text, child.Digest.String()) != 1 {
		t.Fatalf("platform child must appear exactly once:\n%s", text)
	}
	if lines := strings.Split(strings.TrimSpace(text), "\n"); len(lines) != 9 {
		t.Fatalf("expected one table row per identity, got %d lines:\n%s", len(lines), text)
	}
	// Reordering captured metadata and aliases must not change the report.
	metadata.Roots[0], metadata.Roots[2] = metadata.Roots[2], metadata.Roots[0]
	metadata.Origins[0], metadata.Origins[2] = metadata.Origins[2], metadata.Origins[0]
	metadata.Plugins[0], metadata.Plugins[1] = metadata.Plugins[1], metadata.Plugins[0]
	dependencies := metadata.Dependencies[0].Dependencies
	dependencies[0], dependencies[1] = dependencies[1], dependencies[0]
	result.Metadata = metadata
	var reordered bytes.Buffer
	if err := writeCreatePreview(&reordered, result); err != nil {
		t.Fatal(err)
	}
	if reordered.String() != text {
		t.Fatalf("metadata order changed report:\n%s\n---\n%s", text, reordered.String())
	}
}

func TestCreatePreviewEscapesAliasControls(t *testing.T) {
	descriptor := previewDescriptor("selected input")
	alias := "production\nforged row\t\x1b[31m"
	result := bale.CreateResult{Descriptor: previewDescriptor("bundle"), Metadata: bale.Metadata{
		Recipe: bale.Recipe{Include: []bale.Selection{{As: alias}}},
		Roots:  []bale.BundleRoot{{Input: 0, Repository: "registry.example/app", Reference: "v1", Descriptor: descriptor}},
	}}
	var out bytes.Buffer
	if err := writeCreatePreview(&out, result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `include[0] (as "production\nforged row\t\x1b[31m")`) {
		t.Fatalf("alias was not quoted for display: %q", out.String())
	}
	for _, control := range []string{"\nforged row", "\t", "\x1b"} {
		if strings.Contains(out.String(), control) {
			t.Fatalf("alias injected a control character: %q", out.String())
		}
	}
	if result.Metadata.Recipe.Include[0].As != alias {
		t.Fatal("preview modified the stored alias")
	}
}

func TestCreatePreviewOriginsAreAuthoritative(t *testing.T) {
	// Metadata does not list opaque configs/layers as origins. A renderer must
	// not use descriptor size or media type to invent additional artifact rows.
	descriptor := previewDescriptor("opaque-looking origin")
	descriptor.MediaType = "application/octet-stream"
	result := bale.CreateResult{Descriptor: previewDescriptor("bundle"), Metadata: bale.Metadata{
		Origins: []bale.Origin{{Repository: "registry.example/artifact", Descriptor: descriptor}},
	}}
	var output bytes.Buffer
	if err := writeCreatePreview(&output, result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Artifacts: 1 unique") || !strings.Contains(output.String(), "1 content") {
		t.Fatal(output.String())
	}
	if strings.Contains(output.String(), "bytes") || strings.Contains(output.String(), "100") {
		t.Fatalf("preview must not present envelope sizes as payload accounting: %s", output.String())
	}
}

func TestCreatePreviewEmpty(t *testing.T) {
	var output bytes.Buffer
	if err := writeCreatePreview(&output, bale.CreateResult{Descriptor: previewDescriptor("bundle")}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Artifacts: 0 unique") || strings.Contains(output.String(), "ARTIFACT\t") {
		t.Fatal(output.String())
	}
}

func TestCreatePreviewRepositoryIdentity(t *testing.T) {
	descriptor := previewDescriptor("same bytes")
	result := bale.CreateResult{Descriptor: previewDescriptor("bundle"), Metadata: bale.Metadata{
		Roots: []bale.BundleRoot{{Input: 0, Repository: "registry.example/first", Reference: "all", Descriptor: descriptor}},
		Origins: []bale.Origin{
			{Repository: "registry.example/first", Descriptor: descriptor},
			{Repository: "registry.example/first", Descriptor: descriptor},
			{Repository: "registry.example/second", Descriptor: descriptor},
		},
	}}
	var output bytes.Buffer
	if err := writeCreatePreview(&output, result); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Artifacts: 2 unique", "1 input", "1 content", "registry.example/first:all", "registry.example/second@" + descriptor.Digest.String()} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("missing %q:\n%s", expected, output.String())
		}
	}
}

func TestCreatePreviewPreparationFailureWritesNothing(t *testing.T) {
	var output bytes.Buffer
	result := bale.CreateResult{Metadata: bale.Metadata{Origins: []bale.Origin{
		{Repository: "registry.example/artifact", Descriptor: previewDescriptor("valid")},
		{Repository: "", Descriptor: previewDescriptor("missing repository")},
	}}}
	if err := writeCreatePreview(&output, result); err == nil {
		t.Fatal("expected preparation error")
	}
	if output.Len() != 0 {
		t.Fatalf("partial report emitted: %s", output.String())
	}
}

func TestCreatePreviewOCIReferences(t *testing.T) {
	for _, repository := range []string{"registry.example/team/plugin", "registry.example:5000/team/plugin", "[::1]:5000/team/plugin"} {
		for _, reference := range []string{"latest", "all", previewDescriptor("plugin").Digest.String()} {
			t.Run(repository+"/"+reference, func(t *testing.T) {
				selector, err := previewReference(repository, reference)
				if err != nil {
					t.Fatal(err)
				}
				gotRepository, gotSelector, err := previewSelection(selector)
				if err != nil || gotRepository != repository || gotSelector != selector {
					t.Fatalf("reference round trip: %q %q %v", gotRepository, gotSelector, err)
				}
			})
		}
	}
	if _, err := previewReference("registry.example/plugin:unexpected", "latest"); err == nil {
		t.Fatal("repository containing a selector accepted")
	}
	if _, err := previewReference("registry.example/plugin", "not a tag"); err == nil {
		t.Fatal("invalid tag accepted")
	}
}

func TestCreatePreviewInvalidSelectorsWriteNothing(t *testing.T) {
	descriptor := previewDescriptor("artifact")
	for _, selector := range []string{"not-a-reference", "registry.example/UpperCase:latest", "registry.example/plugin@sha256:bad", "registry.example/plugin"} {
		for _, role := range []string{"plugin", "dependency"} {
			t.Run(role+"/"+selector, func(t *testing.T) {
				result := bale.CreateResult{Metadata: bale.Metadata{Origins: []bale.Origin{{Repository: "registry.example/valid", Descriptor: descriptor}}}}
				if role == "plugin" {
					result.Metadata.Plugins = []bale.PluginResolution{{Ref: selector, Descriptor: descriptor}}
				} else {
					result.Metadata.Dependencies = []bale.DependencyResolution{{
						Repository: "registry.example/valid", Subject: descriptor,
						Dependencies: []bale.ResolvedDependency{{Ref: selector, Descriptor: descriptor}},
					}}
				}
				var output bytes.Buffer
				if err := writeCreatePreview(&output, result); err == nil {
					t.Fatal("malformed selector accepted")
				}
				if output.Len() != 0 {
					t.Fatalf("partial report emitted: %s", output.String())
				}
			})
		}
	}
}

type previewFailWriter struct{ err error }

func (w previewFailWriter) Write([]byte) (int, error) { return 0, w.err }

type previewShortWriter struct{}

func (previewShortWriter) Write(data []byte) (int, error) { return len(data) / 2, nil }

func TestCreatePreviewWriterFailure(t *testing.T) {
	want := errors.New("preview output unavailable")
	if err := writeCreatePreview(previewFailWriter{want}, bale.CreateResult{}); !errors.Is(err, want) {
		t.Fatalf("expected output error, got %v", err)
	}
	if err := writeCreatePreview(previewShortWriter{}, bale.CreateResult{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected short write, got %v", err)
	}
}
