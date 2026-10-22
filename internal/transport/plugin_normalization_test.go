package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
)

func TestConfiguredPluginDeclarationCanonicalIdentity(t *testing.T) {
	r, repo, s := testRegistry(t)
	subject := object(t, r.store, "chart")
	image := object(t, r.store, "runtime")
	r.tags["chart"], r.tags["runtime"] = subject, image
	capturePluginFixture(t, r)
	dependencyDeclaration(t, r, image, "runtime-profile")
	input := dependencySelection(repo + ":chart")
	input.Plugin = &config.Plugin{Ref: repo + ":plugin", Input: json.RawMessage(`{}`)}

	for _, tc := range []struct {
		name        string
		declaration dependency.Declaration
		payload     string
	}{
		{"default empty", dependency.Declaration{}, `{"schemaVersion":1,"dependencies":[]}`},
		{"duplicate selectors", dependency.Declaration{Dependencies: []dependency.Reference{
			{Ref: repo + ":runtime", Select: &dependency.Selector{Context: "runtime-profile"}, Follow: "auto"},
			{Ref: repo + ":runtime", Select: &dependency.Selector{Context: "runtime-profile"}},
		}}, fmt.Sprintf(`{"schemaVersion":1,"dependencies":[{"ref":%q,"select":{"context":"runtime-profile"}}]}`, repo+":runtime")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canonical, err := dependency.Decode([]byte(tc.payload))
			if err != nil {
				t.Fatal(err)
			}
			expected, err := PrepareDependencyDeclaration(ctx, Root{Repository: repo, Descriptor: subject}, canonical)
			if err != nil {
				t.Fatal(err)
			}
			s.pluginRunner = func(context.Context, PluginRequest) (dependency.Declaration, error) {
				return tc.declaration, nil
			}
			g, snapshot, err := s.capture(ctx, config.Recipe{Include: []config.Selection{input}})
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Metadata.Plugins) != 1 {
				t.Fatal("missing plugin provenance")
			}
			actual := snapshot.Metadata.Plugins[0].Declaration
			if !content.Equal(actual, expected.Descriptor) {
				t.Fatalf("attachment identity differs: actual=%+v expected=%+v", actual, expected.Descriptor)
			}
			actualBytes, err := content.FetchAll(ctx, g, actual)
			if err != nil {
				t.Fatal(err)
			}
			expectedBytes, err := content.FetchAll(ctx, expected.Store, expected.Descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actualBytes, expectedBytes) {
				t.Fatal("attachment bytes differ")
			}
			var manifest v1.Manifest
			if err := json.Unmarshal(actualBytes, &manifest); err != nil {
				t.Fatal(err)
			}
			if len(manifest.Layers) != 1 {
				t.Fatal("expected one declaration payload")
			}
			payload, err := content.FetchAll(ctx, g, manifest.Layers[0])
			if err != nil {
				t.Fatal(err)
			}
			if string(payload) != tc.payload {
				t.Fatalf("noncanonical payload: %s", payload)
			}
		})
	}
}

func TestConfiguredPluginInvalidDeclarationClassification(t *testing.T) {
	r, repo, s := testRegistry(t)
	r.tags["chart"] = object(t, r.store, "chart")
	capturePluginFixture(t, r)
	input := dependencySelection(repo + ":chart")
	input.Plugin = &config.Plugin{Ref: repo + ":plugin", Input: json.RawMessage(`{}`)}
	for _, declaration := range []dependency.Declaration{
		{SchemaVersion: 99},
		{Dependencies: []dependency.Reference{{Ref: "invalid"}}},
		{Dependencies: []dependency.Reference{{Ref: repo + ":runtime", Select: &dependency.Selector{}}}},
	} {
		s.pluginRunner = func(context.Context, PluginRequest) (dependency.Declaration, error) {
			return declaration, nil
		}
		if _, _, err := s.capture(ctx, config.Recipe{Include: []config.Selection{input}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid declaration lost classification: %v", err)
		}
	}
}
