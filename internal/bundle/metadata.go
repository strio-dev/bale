package bundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/strio-dev/bale/internal/config"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry"
)

// EncodeMetadata produces compact, deterministic JSON without changing caller
// owned slices. Discovery order is not part of a snapshot's identity.
func EncodeMetadata(m Metadata) ([]byte, error) {
	// Round-tripping makes nested recipe/descriptor slices and maps independent.
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out Metadata
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.SchemaVersion == 0 {
		out.SchemaVersion = MetadataVersion
	}
	if out.Roots == nil {
		out.Roots = []Root{}
	}
	if out.Origins == nil {
		out.Origins = []Origin{}
	}
	if out.Referrers == nil {
		out.Referrers = []Association{}
	}
	normalizedRecipe, err := config.Normalize(out.Recipe)
	if err != nil {
		return nil, err
	}
	out.Recipe = normalizedRecipe
	if err := normalizeDependencyMetadata(&out); err != nil {
		return nil, err
	}
	sortMetadata(&out)
	if err := validateMetadata(out); err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

func DecodeMetadata(raw []byte) (Metadata, error) {
	if err := validateMetadataJSON(raw); err != nil {
		return Metadata{}, fmt.Errorf("metadata: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Metadata
	if err := dec.Decode(&m); err != nil {
		return Metadata{}, fmt.Errorf("metadata: %w", err)
	}
	if m.Roots == nil || m.Origins == nil || m.Referrers == nil {
		return Metadata{}, fmt.Errorf("metadata roots, origins and referrers must be arrays")
	}
	if err := validateMetadata(m); err != nil {
		return Metadata{}, err
	}
	return m, nil
}

func sortMetadata(m *Metadata) {
	sort.Slice(m.Plugins, func(i, j int) bool { return pluginResolutionKey(m.Plugins[i]) < pluginResolutionKey(m.Plugins[j]) })
	sort.Slice(m.Roots, func(i, j int) bool {
		a, b := m.Roots[i], m.Roots[j]
		if a.Input != b.Input {
			return a.Input < b.Input
		}
		if a.Repository != b.Repository {
			return a.Repository < b.Repository
		}
		if a.Reference != b.Reference {
			return a.Reference < b.Reference
		}
		return a.Descriptor.Digest < b.Descriptor.Digest
	})
	sort.Slice(m.Origins, func(i, j int) bool {
		a, b := m.Origins[i], m.Origins[j]
		if a.Repository != b.Repository {
			return a.Repository < b.Repository
		}
		return a.Descriptor.Digest < b.Descriptor.Digest
	})
	sort.Slice(m.Referrers, func(i, j int) bool {
		a, b := m.Referrers[i], m.Referrers[j]
		if a.Repository != b.Repository {
			return a.Repository < b.Repository
		}
		if a.Subject.Digest != b.Subject.Digest {
			return a.Subject.Digest < b.Subject.Digest
		}
		return a.Descriptor.Digest < b.Descriptor.Digest
	})
}

func validRepository(repo string) error {
	r, err := registry.ParseReference(repo)
	if err != nil || r.Reference != "" {
		return fmt.Errorf("invalid source repository %q", repo)
	}
	return nil
}

func coordinate(repo string, d digest.Digest) string { return repo + "@" + d.String() }

func validateMetadata(m Metadata) error {
	if m.SchemaVersion != MetadataVersion {
		return fmt.Errorf("unsupported metadata schemaVersion %d", m.SchemaVersion)
	}
	if err := config.Validate(m.Recipe); err != nil {
		return fmt.Errorf("metadata recipe: %w", err)
	}
	if len(m.Roots) == 0 {
		return fmt.Errorf("metadata needs at least one root")
	}
	seen := map[digest.Digest]ocispec.Descriptor{}
	check := func(d ocispec.Descriptor) error {
		if err := validateDescriptor(d); err != nil {
			return err
		}
		if !IsEnvelope(d.MediaType) {
			return fmt.Errorf("origin/root/referrer must be a supported envelope")
		}
		if old, ok := seen[d.Digest]; ok && !sameDescriptor(old, d) {
			return fmt.Errorf("conflicting metadata descriptors for %s", d.Digest)
		}
		seen[d.Digest] = d
		return nil
	}
	origins := map[string]ocispec.Descriptor{}
	for _, o := range m.Origins {
		if err := validRepository(o.Repository); err != nil {
			return err
		}
		if err := check(o.Descriptor); err != nil {
			return err
		}
		key := coordinate(o.Repository, o.Descriptor.Digest)
		if _, ok := origins[key]; ok {
			return fmt.Errorf("duplicate origin %s", key)
		}
		origins[key] = o.Descriptor
	}
	rootKeys := map[string]bool{}
	inputs := make([]bool, len(m.Recipe.Include))
	for _, r := range m.Roots {
		if r.Input < 0 || r.Input >= len(m.Recipe.Include) {
			return fmt.Errorf("root input out of range")
		}
		inputs[r.Input] = true
		if err := validRepository(r.Repository); err != nil {
			return err
		}
		if err := check(r.Descriptor); err != nil {
			return err
		}
		if _, ok := origins[coordinate(r.Repository, r.Descriptor.Digest)]; !ok {
			return fmt.Errorf("root has no matching origin")
		}
		s := m.Recipe.Include[r.Input]
		if s.Ref != "" {
			ref, _ := registry.ParseReference(s.Ref)
			if r.Repository != ref.Registry+"/"+ref.Repository || r.Reference != ref.Reference {
				return fmt.Errorf("root does not match recipe ref")
			}
		} else {
			if r.Repository != s.Repository {
				return fmt.Errorf("root does not match recipe repository")
			}
			if err := (registry.Reference{Reference: r.Reference}).ValidateReferenceAsTag(); err != nil {
				return fmt.Errorf("repository root reference must be a tag: %w", err)
			}
			if len(*s.Tags) > 0 {
				found := false
				for _, tag := range *s.Tags {
					found = found || tag == r.Reference
				}
				if !found {
					return fmt.Errorf("root tag was not selected")
				}
			}
		}
		if err := (registry.Reference{Reference: r.Reference}).ValidateReference(); err != nil {
			return fmt.Errorf("invalid root reference: %w", err)
		}
		if d, err := digest.Parse(r.Reference); err == nil && d != r.Descriptor.Digest {
			return fmt.Errorf("digest-pinned root changed digest")
		}
		key := fmt.Sprintf("%d:%s:%s", r.Input, r.Repository, r.Reference)
		if rootKeys[key] {
			return fmt.Errorf("duplicate root coordinate")
		}
		rootKeys[key] = true
	}
	for i, ok := range inputs {
		if !ok {
			return fmt.Errorf("recipe input %d has no captured roots", i)
		}
	}
	for i, s := range m.Recipe.Include {
		if s.Tags == nil {
			continue
		}
		for _, tag := range *s.Tags {
			key := fmt.Sprintf("%d:%s:%s", i, s.Repository, tag)
			if !rootKeys[key] {
				return fmt.Errorf("recipe input %d selected tag %q was not captured", i, tag)
			}
		}
	}
	associations := map[string]bool{}
	for _, a := range m.Referrers {
		if err := validRepository(a.Repository); err != nil {
			return err
		}
		if err := check(a.Subject); err != nil {
			return err
		}
		if err := check(a.Descriptor); err != nil {
			return err
		}
		if _, ok := origins[coordinate(a.Repository, a.Subject.Digest)]; !ok {
			return fmt.Errorf("referrer subject has no matching origin")
		}
		if _, ok := origins[coordinate(a.Repository, a.Descriptor.Digest)]; !ok {
			return fmt.Errorf("referrer has no matching origin")
		}
		key := coordinate(a.Repository, a.Subject.Digest) + "/" + a.Descriptor.Digest.String()
		if associations[key] {
			return fmt.Errorf("duplicate referrer association")
		}
		associations[key] = true
	}
	for _, r := range m.Roots {
		for _, required := range m.Recipe.Include[r.Input].Referrers.Require {
			key := coordinate(r.Repository, r.Descriptor.Digest) + "/" + required.Digest.String()
			if !associations[key] {
				return fmt.Errorf("required referrer %s missing for root %s", required.Digest, r.Descriptor.Digest)
			}
		}
	}
	if err := validatePluginMetadata(m, origins, check); err != nil {
		return err
	}
	return validateDependencyMetadata(m, origins, check)
}
