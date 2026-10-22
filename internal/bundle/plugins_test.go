package bundle

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
)

func pluginFixture(t *testing.T) (testStore, Metadata) {
	t.Helper()
	s, m := dependencyFixture(t)
	plugin := artifact(t, s, "portable WASI", nil)
	// An index plugin exercises retention of its entire strong closure.
	index := add(t, s, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{plugin}})
	m.Recipe.Include[0].Plugin = &config.Plugin{Ref: "example.test/plugins/helm:1", Input: json.RawMessage(`{"number":9007199254740993,"values":{"optional":null}}`), Blobs: map[string]config.PluginBlob{"chart": {MediaType: "example/chart"}}}
	m.Plugins = []PluginResolution{{Input: 0, Repository: m.Roots[0].Repository, Subject: m.Roots[0].Descriptor, Ref: m.Recipe.Include[0].Plugin.Ref, Descriptor: index, Declaration: *m.Dependencies[0].Declaration}}
	m.Origins = append(m.Origins, Origin{Repository: "example.test/plugins/helm", Descriptor: index}, Origin{Repository: "example.test/plugins/helm", Descriptor: plugin})
	policy, err := RootDependencyPolicy(m, m.Roots[0])
	if err != nil {
		t.Fatal(err)
	}
	m.Dependencies[0].Policy = policy
	return s, m
}

func TestPluginSnapshotRetainsModuleAndMetadata(t *testing.T) {
	s, m := pluginFixture(t)
	snapshot, err := BuildSnapshot(context.Background(), s, m)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSnapshotGraph(context.Background(), s, snapshot.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Metadata.Plugins) != 1 || !strings.Contains(string(loaded.Metadata.Recipe.Include[0].Plugin.Input), `9007199254740993`) || !strings.Contains(string(loaded.Metadata.Recipe.Include[0].Plugin.Input), `null`) {
		t.Fatal("plugin input/provenance lost")
	}
	var index ocispec.Index
	if err := json.Unmarshal(s[snapshot.Descriptor.Digest], &index); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range index.Manifests {
		found = found || d.Digest == m.Plugins[0].Descriptor.Digest
	}
	if !found {
		t.Fatal("plugin not directly retained")
	}
	delete(s, m.Plugins[0].Descriptor.Digest)
	if _, err := LoadSnapshotGraph(context.Background(), s, snapshot.Descriptor); err == nil {
		t.Fatal("accepted missing plugin graph")
	}
}

func TestPluginMetadataRejectsTampering(t *testing.T) {
	cases := map[string]func(*Metadata){
		"missing record":          func(m *Metadata) { m.Plugins = nil },
		"duplicate record":        func(m *Metadata) { m.Plugins = append(m.Plugins, m.Plugins[0]) },
		"wrong input":             func(m *Metadata) { m.Plugins[0].Input = 7 },
		"unconfigured":            func(m *Metadata) { m.Recipe.Include[0].Plugin = nil },
		"wrong repo":              func(m *Metadata) { m.Plugins[0].Repository = "elsewhere.example/repo" },
		"wrong ref":               func(m *Metadata) { m.Plugins[0].Ref = "example.test/plugins/helm:2" },
		"wrong subject":           func(m *Metadata) { m.Plugins[0].Subject = m.Plugins[0].Descriptor },
		"missing origin":          func(m *Metadata) { m.Origins = m.Origins[:3] },
		"missing association":     func(m *Metadata) { m.Referrers = nil },
		"wrong declaration":       func(m *Metadata) { m.Plugins[0].Declaration = m.Plugins[0].Descriptor },
		"wrong resolution policy": func(m *Metadata) { m.Dependencies[0].Policy = dependency.Policy{Follow: "auto"} },
		"changed pinned digest": func(m *Metadata) {
			ref := "example.test/plugins/helm@" + m.Roots[0].Descriptor.Digest.String()
			m.Recipe.Include[0].Plugin.Ref = ref
			m.Plugins[0].Ref = ref
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, m := pluginFixture(t)
			mutate(&m)
			if _, err := EncodeMetadata(m); err == nil {
				t.Fatal("accepted tampered plugin metadata")
			}
		})
	}
}

func TestPluginProfilesSelectIndependentDeclarations(t *testing.T) {
	s, m := pluginFixture(t)
	second := m.Recipe.Include[0]
	p := *second.Plugin
	p.Input = json.RawMessage(`{"profile":"second"}`)
	second.Plugin = &p
	second.As = "second"
	m.Recipe.Include = append(m.Recipe.Include, second)
	root := m.Roots[0]
	root.Input = 1
	m.Roots = append(m.Roots, root)
	declaration := declarationArtifact(t, s, root.Descriptor, dependency.Declaration{Context: "second", Dependencies: []dependency.Reference{}})
	m.Origins = append(m.Origins, Origin{Repository: root.Repository, Descriptor: declaration})
	m.Referrers = append(m.Referrers, Association{Repository: root.Repository, Subject: root.Descriptor, Descriptor: declaration})
	resolution := m.Plugins[0]
	resolution.Input = 1
	resolution.Declaration = declaration
	m.Plugins = append(m.Plugins, resolution)
	policy, err := RootDependencyPolicy(m, root)
	if err != nil {
		t.Fatal(err)
	}
	m.Dependencies = append(m.Dependencies, DependencyResolution{Repository: root.Repository, Subject: root.Descriptor, Policy: policy, Outcome: "selected", Declaration: &declaration, Dependencies: []ResolvedDependency{}})
	if _, err := BuildSnapshot(context.Background(), s, m); err != nil {
		t.Fatal(err)
	}
	first, _ := RootDependencyPolicy(m, m.Roots[0])
	if first.Select.Digest == policy.Select.Digest {
		t.Fatal("profiles share declaration selection")
	}
	// A second tag for the same input and digest reuses the single record.
	tags := []string{"stable", "other"}
	m.Recipe.Include[0].Ref = ""
	m.Recipe.Include[0].Repository = root.Repository
	m.Recipe.Include[0].Tags = &tags
	alias := m.Roots[0]
	alias.Reference = "other"
	m.Roots = append(m.Roots, alias)
	if _, err := BuildSnapshot(context.Background(), s, m); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataNullExceptionIsOnlyPluginInput(t *testing.T) {
	_, m := pluginFixture(t)
	raw, err := EncodeMetadata(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, alter := range []string{strings.Replace(string(raw), `"plugins":[`, `"plugins":null,"ignored":[`, 1), strings.Replace(string(raw), `"input":{"number":9007199254740993,"values":{"optional":null}}`, `"input":null`, 1), strings.Replace(string(raw), `"number":9007199254740993`, `"number":1,"number":2`, 1)} {
		if _, err := DecodeMetadata([]byte(alter)); err == nil {
			t.Fatal("accepted invalid metadata null/duplicate")
		}
	}
}

func TestPluginReferenceSharesRootAndDependencyPins(t *testing.T) {
	cases := []struct{ name, ref, repository string }{
		{"root", "example.test/source:stable", "example.test/source"},
		{"dependency", "other.example/images/app:stable", "other.example/images/app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, m := pluginFixture(t)
			m.Recipe.Include[0].Plugin.Ref = tc.ref
			m.Plugins[0].Ref = tc.ref
			m.Origins = append(m.Origins, Origin{Repository: tc.repository, Descriptor: m.Plugins[0].Descriptor})
			if _, err := EncodeMetadata(m); err == nil || !strings.Contains(err.Error(), "source reference resolved inconsistently") {
				t.Fatalf("encode accepted conflicting %s/plugin pins or failed for another reason: %v", tc.name, err)
			}
			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeMetadata(raw); err == nil || !strings.Contains(err.Error(), "source reference resolved inconsistently") {
				t.Fatalf("decode accepted conflicting %s/plugin pins or failed for another reason: %v", tc.name, err)
			}
		})
	}
}
