package bundle

import (
	"bytes"
	"encoding/json"
	"fmt"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/registry"
)

func pluginResolutionKey(p PluginResolution) string {
	return fmt.Sprintf("%d/%s", p.Input, coordinate(p.Repository, p.Subject.Digest))
}

// RootDependencyPolicy selects the generated declaration for this exact input
// and subject. Different render profiles can therefore coexist in one bundle.
func RootDependencyPolicy(m Metadata, root Root) (dependency.Policy, error) {
	if root.Input < 0 || root.Input >= len(m.Recipe.Include) {
		return dependency.Policy{}, fmt.Errorf("root input out of range")
	}
	selection := m.Recipe.Include[root.Input]
	if selection.Plugin == nil {
		return dependency.NormalizePolicy(selection.Dependencies)
	}
	key := pluginResolutionKey(PluginResolution{Input: root.Input, Repository: root.Repository, Subject: root.Descriptor})
	var found *PluginResolution
	for i := range m.Plugins {
		p := &m.Plugins[i]
		if pluginResolutionKey(*p) == key {
			if found != nil {
				return dependency.Policy{}, fmt.Errorf("duplicate plugin resolution")
			}
			found = p
		}
	}
	if found == nil {
		return dependency.Policy{}, fmt.Errorf("missing plugin resolution for root %s", key)
	}
	return dependency.Policy{Follow: "auto", Select: &dependency.Selector{Digest: found.Declaration.Digest}}, nil
}

func validatePluginMetadata(m Metadata, origins map[string]ocispec.Descriptor, check func(ocispec.Descriptor) error) error {
	expected := map[string]Root{}
	for _, root := range m.Roots {
		if m.Recipe.Include[root.Input].Plugin != nil {
			key := pluginResolutionKey(PluginResolution{Input: root.Input, Repository: root.Repository, Subject: root.Descriptor})
			expected[key] = root
		}
	}
	seen := map[string]bool{}
	for _, p := range m.Plugins {
		key := pluginResolutionKey(p)
		root, ok := expected[key]
		if !ok {
			return fmt.Errorf("plugin resolution does not match a configured root")
		}
		if seen[key] {
			return fmt.Errorf("duplicate plugin resolution")
		}
		seen[key] = true
		if !sameDescriptor(root.Descriptor, p.Subject) {
			return fmt.Errorf("plugin subject differs from root descriptor")
		}
		selection := m.Recipe.Include[p.Input].Plugin
		if p.Ref != selection.Ref {
			return fmt.Errorf("plugin resolution ref differs from recipe")
		}
		if err := dependency.ValidateReference(p.Ref); err != nil {
			return err
		}
		ref, _ := registry.ParseReference(p.Ref)
		if pinned, err := digest.Parse(ref.Reference); err == nil && pinned != p.Descriptor.Digest {
			return fmt.Errorf("digest-pinned plugin changed digest")
		}
		for _, d := range []ocispec.Descriptor{p.Subject, p.Descriptor, p.Declaration} {
			if err := check(d); err != nil {
				return err
			}
		}
		origin, ok := origins[coordinate(ref.Registry+"/"+ref.Repository, p.Descriptor.Digest)]
		if !ok || !sameDescriptor(origin, p.Descriptor) {
			return fmt.Errorf("plugin has no matching origin")
		}
		associated := false
		for _, a := range m.Referrers {
			if a.Repository == p.Repository && sameDescriptor(a.Subject, p.Subject) && sameDescriptor(a.Descriptor, p.Declaration) {
				associated = true
				break
			}
		}
		if !associated {
			return fmt.Errorf("plugin declaration has no matching referrer association")
		}
		selected := false
		policy, err := RootDependencyPolicy(m, root)
		if err != nil {
			return err
		}
		for _, r := range m.Dependencies {
			if r.Repository == p.Repository && sameDescriptor(r.Subject, p.Subject) && policyKey(r.Policy) == policyKey(policy) && r.Outcome == "selected" && r.Declaration != nil && sameDescriptor(*r.Declaration, p.Declaration) {
				selected = true
				break
			}
		}
		if !selected {
			return fmt.Errorf("plugin declaration has no matching selected dependency resolution")
		}
	}
	if len(expected) != len(seen) {
		return fmt.Errorf("configured plugin root has no resolution")
	}
	return nil
}

// Plugin input is the sole metadata subtree allowed to retain JSON nulls.
// Validate duplicates before inspecting paths so decoding cannot hide them.
func validateMetadataJSON(raw []byte) error {
	if err := uniqueJSON(raw, false); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return err
	}
	var walk func(any, []string) error
	walk = func(value any, path []string) error {
		if len(path) == 5 && path[0] == "recipe" && path[1] == "include" && path[3] == "plugin" && path[4] == "input" {
			if _, ok := value.(map[string]any); !ok {
				return fmt.Errorf("plugin.input must be an object")
			}
			return nil
		}
		switch v := value.(type) {
		case nil:
			return fmt.Errorf("null is not allowed")
		case map[string]any:
			for key, child := range v {
				if err := walk(child, append(append([]string{}, path...), key)); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range v {
				if err := walk(child, append(append([]string{}, path...), "[]")); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(value, nil)
}
