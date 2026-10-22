package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
)

// LoadDependencyDeclaration validates a declaration's OCI envelope, exact
// subject and JSON payload. It does not discover or resolve dependencies.
func LoadDependencyDeclaration(ctx context.Context, store content.ReadOnlyStorage, desc, subject ocispec.Descriptor) (dependency.Declaration, error) {
	fail := func(err error) (dependency.Declaration, error) {
		return dependency.Declaration{}, fmt.Errorf("dependency declaration %s: %w", desc.Digest, err)
	}
	if desc.MediaType != ocispec.MediaTypeImageManifest {
		return fail(fmt.Errorf("must be an OCI manifest"))
	}
	if desc.Size > dependency.MaxBytes {
		return fail(fmt.Errorf("manifest exceeds size limit"))
	}
	raw, err := fetchBytes(ctx, store, desc)
	if err != nil {
		return fail(err)
	}
	env, err := ParseEnvelope(desc, raw)
	if err != nil {
		return fail(err)
	}
	if env.ArtifactType != dependency.ArtifactType {
		return fail(fmt.Errorf("unexpected artifactType"))
	}
	if env.Subject == nil || !sameDescriptor(*env.Subject, subject) || env.Subject.MediaType != subject.MediaType {
		return fail(fmt.Errorf("subject does not match"))
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fail(err)
	}
	if manifest.Config.MediaType != ocispec.MediaTypeEmptyJSON || manifest.Config.Size != 2 {
		return fail(fmt.Errorf("config must be the OCI empty JSON object"))
	}
	empty, err := fetchBytes(ctx, store, manifest.Config)
	if err != nil {
		return fail(err)
	}
	if !bytes.Equal(empty, []byte("{}")) {
		return fail(fmt.Errorf("config must be the OCI empty JSON object"))
	}
	var payload *ocispec.Descriptor
	for i := range manifest.Layers {
		layer := &manifest.Layers[i]
		if layer.MediaType == dependency.ArtifactType {
			if payload != nil {
				return fail(fmt.Errorf("multiple declaration layers"))
			}
			payload = layer
		} else if IsEnvelope(layer.MediaType) {
			return fail(fmt.Errorf("additional layers must be opaque"))
		}
	}
	if payload == nil {
		return fail(fmt.Errorf("missing declaration layer"))
	}
	if payload.Size > dependency.MaxBytes {
		return fail(fmt.Errorf("payload exceeds size limit"))
	}
	raw, err = fetchBytes(ctx, store, *payload)
	if err != nil {
		return fail(err)
	}
	out, err := dependency.Decode(raw)
	if err != nil {
		return fail(err)
	}
	return out, nil
}

func policyKey(p dependency.Policy) string { raw, _ := json.Marshal(p); return string(raw) }
func dependencyResolutionKey(r DependencyResolution) string {
	return coordinate(r.Repository, r.Subject.Digest) + "/" + policyKey(r.Policy)
}
func resolvedDependencyKey(r ResolvedDependency) string { return r.Ref + "/" + policyKey(r.Policy) }

func normalizeDependencyMetadata(m *Metadata) error {
	for i := range m.Dependencies {
		r := &m.Dependencies[i]
		p, err := dependency.NormalizePolicy(r.Policy)
		if err != nil {
			return err
		}
		r.Policy = p
		if r.Dependencies == nil {
			r.Dependencies = []ResolvedDependency{}
		}
		for j := range r.Dependencies {
			p, err := dependency.NormalizePolicy(r.Dependencies[j].Policy)
			if err != nil {
				return err
			}
			r.Dependencies[j].Policy = p
		}
		sort.Slice(r.Dependencies, func(i, j int) bool {
			return resolvedDependencyKey(r.Dependencies[i]) < resolvedDependencyKey(r.Dependencies[j])
		})
	}
	sort.Slice(m.Dependencies, func(i, j int) bool {
		return dependencyResolutionKey(m.Dependencies[i]) < dependencyResolutionKey(m.Dependencies[j])
	})
	return nil
}

func validateDependencyMetadata(m Metadata, origins map[string]ocispec.Descriptor, check func(ocispec.Descriptor) error) error {
	states := map[string]bool{}
	resolvedReferences := map[string]ocispec.Descriptor{}
	for _, plugin := range m.Plugins {
		if previous, ok := resolvedReferences[plugin.Ref]; ok && !sameDescriptor(previous, plugin.Descriptor) {
			return fmt.Errorf("source reference resolved inconsistently within capture")
		}
		resolvedReferences[plugin.Ref] = plugin.Descriptor
	}
	for _, root := range m.Roots {
		ref, _ := registry.ParseReference(root.Repository)
		ref.Reference = root.Reference
		key := ref.String()
		if previous, ok := resolvedReferences[key]; ok && !sameDescriptor(previous, root.Descriptor) {
			return fmt.Errorf("source reference resolved inconsistently within capture")
		}
		resolvedReferences[key] = root.Descriptor
	}
	for _, r := range m.Dependencies {
		if err := validRepository(r.Repository); err != nil {
			return err
		}
		if err := check(r.Subject); err != nil {
			return err
		}
		if _, ok := origins[coordinate(r.Repository, r.Subject.Digest)]; !ok {
			return fmt.Errorf("dependency subject has no matching origin")
		}
		p, err := dependency.NormalizePolicy(r.Policy)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(p, r.Policy) {
			return fmt.Errorf("dependency policy must be normalized")
		}
		key := dependencyResolutionKey(r)
		if states[key] {
			return fmt.Errorf("duplicate dependency resolution state")
		}
		states[key] = true
		if r.Dependencies == nil {
			return fmt.Errorf("resolved dependencies must be an array")
		}
		switch r.Outcome {
		case "selected":
			if p.Follow == "none" || r.Declaration == nil {
				return fmt.Errorf("selected dependency resolution needs enabled policy and declaration")
			}
			if err := check(*r.Declaration); err != nil {
				return err
			}
			if p.Select != nil && p.Select.Digest != "" && p.Select.Digest != r.Declaration.Digest {
				return fmt.Errorf("selected declaration differs from policy digest")
			}
			found := false
			for _, a := range m.Referrers {
				if a.Repository == r.Repository && sameDescriptor(a.Subject, r.Subject) && sameDescriptor(a.Descriptor, *r.Declaration) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("selected dependency declaration has no captured referrer association")
			}
		case "absent":
			if p.Follow == "none" || p.Select != nil || r.Declaration != nil || len(r.Dependencies) != 0 {
				return fmt.Errorf("invalid absent dependency resolution")
			}
		case "disabled":
			if p.Follow != "none" || r.Declaration != nil || len(r.Dependencies) != 0 {
				return fmt.Errorf("invalid disabled dependency resolution")
			}
		default:
			return fmt.Errorf("invalid dependency resolution outcome %q", r.Outcome)
		}
		entries := map[string]bool{}
		for _, d := range r.Dependencies {
			if err := dependency.ValidateReference(d.Ref); err != nil {
				return err
			}
			if err := check(d.Descriptor); err != nil {
				return err
			}
			if previous, ok := resolvedReferences[d.Ref]; ok && !sameDescriptor(previous, d.Descriptor) {
				return fmt.Errorf("source reference resolved inconsistently within capture")
			}
			resolvedReferences[d.Ref] = d.Descriptor
			p, err := dependency.NormalizePolicy(d.Policy)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(p, d.Policy) {
				return fmt.Errorf("resolved dependency policy must be normalized")
			}
			ref, _ := registry.ParseReference(d.Ref)
			repo := ref.Registry + "/" + ref.Repository
			if _, ok := origins[coordinate(repo, d.Descriptor.Digest)]; !ok {
				return fmt.Errorf("resolved dependency has no matching origin")
			}
			if pinned, err := digest.Parse(ref.Reference); err == nil && pinned != d.Descriptor.Digest {
				return fmt.Errorf("digest-pinned dependency changed digest")
			}
			key := resolvedDependencyKey(d)
			if entries[key] {
				return fmt.Errorf("duplicate resolved dependency")
			}
			entries[key] = true
		}
	}
	return nil
}

func validateCapturedDependencies(ctx context.Context, store content.ReadOnlyStorage, m Metadata) error {
	for _, r := range m.Dependencies {
		if r.Outcome != "selected" {
			continue
		}
		declaration, err := LoadDependencyDeclaration(ctx, store, *r.Declaration, r.Subject)
		if err != nil {
			return err
		}
		if r.Policy.Select != nil && r.Policy.Select.Context != "" && r.Policy.Select.Context != declaration.Context {
			return fmt.Errorf("dependency declaration context differs from selected policy")
		}
		expected := map[string]bool{}
		for _, entry := range declaration.Dependencies {
			p := dependency.Policy{Follow: r.Policy.Follow, Select: entry.Select}
			if entry.Follow == "none" {
				p.Follow = "none"
			}
			p, err = dependency.NormalizePolicy(p)
			if err != nil {
				return err
			}
			expected[resolvedDependencyKey(ResolvedDependency{Ref: entry.Ref, Policy: p})] = true
		}
		if len(expected) != len(r.Dependencies) {
			return fmt.Errorf("captured dependencies differ from declaration")
		}
		for _, d := range r.Dependencies {
			if !expected[resolvedDependencyKey(d)] {
				return fmt.Errorf("captured dependency differs from declaration reference or policy")
			}
		}
	}
	return validateDependencyObservations(ctx, store, m)
}

// Captured observations make enabled traversal auditable: an omitted record
// cannot silently turn a declared dependency into a graph-only inclusion.
func validateDependencyObservations(ctx context.Context, store content.ReadOnlyStorage, m Metadata) error {
	states := make(map[string]DependencyResolution, len(m.Dependencies))
	for _, r := range m.Dependencies {
		states[dependencyResolutionKey(r)] = r
	}
	associations := map[string][]Association{}
	for _, a := range m.Referrers {
		key := coordinate(a.Repository, a.Subject.Digest)
		associations[key] = append(associations[key], a)
	}
	envelopes := map[string]Envelope{}
	envelope := func(d ocispec.Descriptor) (Envelope, error) {
		key := d.MediaType + "/" + d.Digest.String()
		if e, ok := envelopes[key]; ok {
			return e, nil
		}
		raw, err := fetchBytes(ctx, store, d)
		if err != nil {
			return Envelope{}, err
		}
		e, err := ParseEnvelope(d, raw)
		if err != nil {
			return Envelope{}, err
		}
		envelopes[key] = e
		return e, nil
	}
	visited := map[string]bool{}
	var walk func(string, ocispec.Descriptor, dependency.Policy, bool) error
	walk = func(repo string, d ocispec.Descriptor, p dependency.Policy, optional bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		p, err := dependency.NormalizePolicy(p)
		if err != nil {
			return err
		}
		state := dependencyResolutionKey(DependencyResolution{Repository: repo, Subject: d, Policy: p})
		visit := fmt.Sprintf("%s/%t", state, optional)
		if visited[visit] {
			return nil
		}
		visited[visit] = true
		e, err := envelope(d)
		if err != nil {
			return err
		}
		if strings.HasPrefix(e.ArtifactType, "application/vnd.bale.dependencies.") {
			return nil
		}
		childPolicy := dependency.Policy{Follow: p.Follow}
		// Native bundle membership has already been captured. Outer referrers are
		// separate selections and retain their caller's traversal policy.
		if e.ArtifactType != BundleArtifactType {
			r, ok := states[state]
			if !ok {
				return fmt.Errorf("missing captured dependency resolution for %s", coordinate(repo, d.Digest))
			}
			if p.Follow == "none" {
				return nil
			}
			for _, target := range r.Dependencies {
				ref, _ := registry.ParseReference(target.Ref)
				if err := walk(ref.Registry+"/"+ref.Repository, target.Descriptor, target.Policy, optional); err != nil {
					return err
				}
			}
			for i, child := range e.Children {
				if IsEnvelopeChild(d, i, child) {
					if err := walk(repo, child, childPolicy, optional); err != nil {
						return err
					}
				}
			}
		}
		if optional && p.Follow != "none" {
			for _, a := range associations[coordinate(repo, d.Digest)] {
				if err := walk(a.Repository, a.Descriptor, childPolicy, optional); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, root := range m.Roots {
		selection := m.Recipe.Include[root.Input]
		p, err := RootDependencyPolicy(m, root)
		if err != nil {
			return err
		}
		optional := selection.Referrers.Include == "all"
		if err := walk(root.Repository, root.Descriptor, p, optional); err != nil {
			return err
		}
		// Required referrers are selected independently of optional discovery.
		for _, required := range selection.Referrers.Require {
			for _, a := range associations[coordinate(root.Repository, root.Descriptor.Digest)] {
				if a.Descriptor.Digest == required.Digest {
					if err := walk(a.Repository, a.Descriptor, dependency.Policy{Follow: p.Follow}, optional); err != nil {
						return err
					}
				}
			}
		}
	}
	// Every record also asserts its own structural and declared dependency
	// traversal even when it is not reachable through an enabled recipe path.
	for _, r := range m.Dependencies {
		if err := walk(r.Repository, r.Subject, r.Policy, false); err != nil {
			return err
		}
	}
	return nil
}
