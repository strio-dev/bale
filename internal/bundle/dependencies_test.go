package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/dependency"
)

func declarationArtifact(t *testing.T, s testStore, subject ocispec.Descriptor, declaration dependency.Declaration) ocispec.Descriptor {
	t.Helper()
	raw, err := dependency.Encode(declaration)
	if err != nil {
		t.Fatal(err)
	}
	payload := add(t, s, dependency.ArtifactType, raw)
	empty := add(t, s, ocispec.MediaTypeEmptyJSON, []byte("{}"))
	return add(t, s, ocispec.MediaTypeImageManifest, ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, ArtifactType: dependency.ArtifactType, Subject: &subject, Config: empty, Layers: []ocispec.Descriptor{payload}})
}

func dependencyFixture(t *testing.T) (testStore, Metadata) {
	t.Helper()
	s := testStore{}
	root := artifact(t, s, "root", nil)
	dep := artifact(t, s, "dependency", nil)
	decl := declarationArtifact(t, s, root, dependency.Declaration{Dependencies: []dependency.Reference{{Ref: "other.example/images/app:stable"}, {Ref: "other.example/images/app:stable"}}})
	m := metadata(root)
	m.Recipe.Include[0].Dependencies.Follow = "auto"
	m.Origins = append(m.Origins, Origin{Repository: "example.test/source", Descriptor: decl}, Origin{Repository: "other.example/images/app", Descriptor: dep})
	m.Referrers = append(m.Referrers, Association{Repository: "example.test/source", Subject: root, Descriptor: decl})
	m.Dependencies = []DependencyResolution{{Repository: "example.test/source", Subject: root, Policy: dependency.Policy{Follow: "auto"}, Outcome: "selected", Declaration: &decl, Dependencies: []ResolvedDependency{{Ref: "other.example/images/app:stable", Descriptor: dep, Policy: dependency.Policy{Follow: "auto"}}}}}
	m.Dependencies = append(m.Dependencies, DependencyResolution{Repository: "other.example/images/app", Subject: dep, Policy: dependency.Policy{Follow: "auto"}, Outcome: "absent", Dependencies: []ResolvedDependency{}})
	return s, m
}

func TestDependencySnapshotMembershipAndLock(t *testing.T) {
	ctx := context.Background()
	s, m := dependencyFixture(t)
	snapshot, err := BuildSnapshot(ctx, s, m)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSnapshotGraph(ctx, s, snapshot.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Metadata.Dependencies) != 2 || len(loaded.Metadata.Dependencies[0].Dependencies) != 1 {
		t.Fatal("lost captured dependency resolution")
	}
	var index ocispec.Index
	if err := json.Unmarshal(s[snapshot.Descriptor.Digest], &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 4 {
		t.Fatalf("want metadata, root, declaration and deduped dependency, got %d", len(index.Manifests))
	}
	delete(s, m.Dependencies[0].Dependencies[0].Descriptor.Digest)
	if _, err := LoadSnapshotGraph(ctx, s, snapshot.Descriptor); err == nil {
		t.Fatal("accepted missing dependency graph")
	}
}

func TestDependencyMetadataRejectsTampering(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Metadata)
	}{
		{"missing association", func(m *Metadata) { m.Referrers = nil }},
		{"missing origin", func(m *Metadata) { m.Origins = m.Origins[:2] }},
		{"digest assertion", func(m *Metadata) {
			m.Dependencies[0].Dependencies[0].Ref = "other.example/images/app@" + m.Roots[0].Descriptor.Digest.String()
		}},
		{"unknown outcome", func(m *Metadata) { m.Dependencies[0].Outcome = "cached" }},
		{"disabled selected", func(m *Metadata) { m.Dependencies[0].Policy.Follow = "none" }},
		{"duplicate state", func(m *Metadata) { m.Dependencies = append(m.Dependencies, m.Dependencies[0]) }},
		{"duplicate resolution", func(m *Metadata) { r := &m.Dependencies[0]; r.Dependencies = append(r.Dependencies, r.Dependencies[0]) }},
		{"wrong declaration", func(m *Metadata) { d := m.Roots[0].Descriptor; m.Dependencies[0].Declaration = &d }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, m := dependencyFixture(t)
			tc.mutate(&m)
			if _, err := EncodeMetadata(m); err == nil {
				t.Fatal("accepted tampered metadata")
			}
		})
	}
}

func TestDependencySnapshotRejectsPayloadMismatch(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Metadata)
	}{
		{"different reference", func(m *Metadata) { m.Dependencies[0].Dependencies[0].Ref = "other.example/images/app:different" }},
		{"missing entry", func(m *Metadata) { m.Dependencies[0].Dependencies = []ResolvedDependency{} }},
		{"different child policy", func(m *Metadata) { m.Dependencies[0].Dependencies[0].Policy.Follow = "none" }},
		{"different context", func(m *Metadata) { m.Dependencies[0].Policy.Select = &dependency.Selector{Context: "production"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, m := dependencyFixture(t)
			tc.mutate(&m)
			if _, err := BuildSnapshot(context.Background(), s, m); err == nil {
				t.Fatal("accepted declaration/resolution mismatch")
			}
		})
	}
}

func TestLoadDependencyDeclarationRejectsInvalidEnvelope(t *testing.T) {
	s, m := dependencyFixture(t)
	good := *m.Dependencies[0].Declaration
	var base ocispec.Manifest
	if err := json.Unmarshal(s[good.Digest], &base); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*ocispec.Manifest)
	}{
		{"no subject", func(x *ocispec.Manifest) { x.Subject = nil }},
		{"wrong subject", func(x *ocispec.Manifest) { d := m.Dependencies[0].Dependencies[0].Descriptor; x.Subject = &d }},
		{"wrong artifactType", func(x *ocispec.Manifest) { x.ArtifactType = "application/example" }},
		{"wrong config", func(x *ocispec.Manifest) { x.Config = add(t, s, ocispec.MediaTypeEmptyJSON, []byte("[]")) }},
		{"missing layer", func(x *ocispec.Manifest) { x.Layers = []ocispec.Descriptor{} }},
		{"duplicate layer", func(x *ocispec.Manifest) { x.Layers = append(x.Layers, x.Layers[0]) }},
		{"extra envelope layer", func(x *ocispec.Manifest) { x.Layers = append(x.Layers, m.Roots[0].Descriptor) }},
		{"oversize payload", func(x *ocispec.Manifest) { x.Layers[0].Size = dependency.MaxBytes + 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := base
			x.Layers = append([]ocispec.Descriptor(nil), base.Layers...)
			tc.mutate(&x)
			d := add(t, s, ocispec.MediaTypeImageManifest, x)
			if _, err := LoadDependencyDeclaration(context.Background(), s, d, m.Roots[0].Descriptor); err == nil {
				t.Fatal("accepted malformed declaration")
			}
		})
	}
	// Inline payloads must still have valid digest/size assertions.
	x := base
	x.Layers = append([]ocispec.Descriptor(nil), base.Layers...)
	x.Layers[0].Data = []byte("tampered")
	d := add(t, s, ocispec.MediaTypeImageManifest, x)
	if _, err := LoadDependencyDeclaration(context.Background(), s, d, m.Roots[0].Descriptor); err == nil {
		t.Fatal("accepted tampered inline data")
	}
}

func TestDependencyMetadataDeterministicAndCallerOwned(t *testing.T) {
	_, m := dependencyFixture(t)
	// One digest may satisfy multiple distinct source selectors without becoming
	// multiple direct bundle children.
	r := &m.Dependencies[0]
	second := r.Dependencies[0]
	second.Ref = "other.example/images/app:another"
	r.Dependencies = append(r.Dependencies, second)
	before, _ := json.Marshal(m)
	first, err := EncodeMetadata(m)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(m)
	if !bytes.Equal(before, after) {
		t.Fatal("mutated caller metadata")
	}
	r.Dependencies[0], r.Dependencies[1] = r.Dependencies[1], r.Dependencies[0]
	other, err := EncodeMetadata(m)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, other) {
		t.Fatal("discovery order changed identity")
	}
	raw := strings.Replace(string(first), `"dependencies":[{`, `"dependencies":null,"unexpected":[{`, 1)
	if _, err := DecodeMetadata([]byte(raw)); err == nil {
		t.Fatal("accepted invalid dependency fields")
	}
}

func TestDependencySnapshotTerminalObservations(t *testing.T) {
	for _, outcome := range []string{"absent", "disabled"} {
		t.Run(outcome, func(t *testing.T) {
			s := testStore{}
			root := artifact(t, s, "root", nil)
			m := metadata(root)
			follow := "auto"
			if outcome == "disabled" {
				follow = "none"
			}
			m.Recipe.Include[0].Dependencies.Follow = follow
			m.Dependencies = []DependencyResolution{{Repository: m.Roots[0].Repository, Subject: root, Policy: dependency.Policy{Follow: follow}, Outcome: outcome}}
			snapshot, err := BuildSnapshot(context.Background(), s, m)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadSnapshotGraph(context.Background(), s, snapshot.Descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Metadata.Dependencies[0].Outcome != outcome {
				t.Fatal("lost terminal dependency observation")
			}
		})
	}
}

func TestDependencyMetadataRejectsInconsistentTagResolution(t *testing.T) {
	s, m := dependencyFixture(t)
	other := artifact(t, s, "different dependency", nil)
	m.Origins = append(m.Origins, Origin{Repository: "other.example/images/app", Descriptor: other})
	r := &m.Dependencies[0]
	second := r.Dependencies[0]
	second.Policy.Follow = "none"
	second.Descriptor = other
	r.Dependencies = append(r.Dependencies, second)
	if _, err := EncodeMetadata(m); err == nil || !strings.Contains(err.Error(), "inconsistently") {
		t.Fatalf("accepted contradictory resolutions: %v", err)
	}
}

func TestDependencyMetadataRejectsRootDependencyTagDisagreement(t *testing.T) {
	_, m := dependencyFixture(t)
	r := &m.Dependencies[0]
	r.Dependencies[0].Ref = "example.test/source:stable"
	m.Origins = append(m.Origins, Origin{Repository: "example.test/source", Descriptor: r.Dependencies[0].Descriptor})
	if _, err := EncodeMetadata(m); err == nil || !strings.Contains(err.Error(), "inconsistently") {
		t.Fatalf("accepted root/dependency disagreement: %v", err)
	}
}

func TestDependencySnapshotRequiresResolutionObservations(t *testing.T) {
	t.Run("declared target", func(t *testing.T) {
		s, m := dependencyFixture(t)
		m.Dependencies = m.Dependencies[:1]
		if _, err := BuildSnapshot(context.Background(), s, m); err == nil || !strings.Contains(err.Error(), "missing captured dependency resolution") {
			t.Fatalf("accepted omitted target observation: %v", err)
		}
	})
	t.Run("explicit root policy", func(t *testing.T) {
		s := testStore{}
		root := artifact(t, s, "root", nil)
		m := metadata(root)
		m.Recipe.Include[0].Dependencies.Follow = "auto"
		if _, err := BuildSnapshot(context.Background(), s, m); err == nil || !strings.Contains(err.Error(), "missing captured dependency resolution") {
			t.Fatalf("accepted omitted root observation: %v", err)
		}
	})
	t.Run("structural child", func(t *testing.T) {
		s := testStore{}
		child := artifact(t, s, "child", nil)
		root := add(t, s, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{child}})
		m := metadata(root)
		m.Origins = append(m.Origins, Origin{Repository: m.Roots[0].Repository, Descriptor: child})
		m.Recipe.Include[0].Dependencies.Follow = "auto"
		m.Dependencies = []DependencyResolution{{Repository: m.Roots[0].Repository, Subject: root, Policy: dependency.Policy{Follow: "auto"}, Outcome: "absent", Dependencies: []ResolvedDependency{}}}
		if _, err := BuildSnapshot(context.Background(), s, m); err == nil || !strings.Contains(err.Error(), "missing captured dependency resolution") {
			t.Fatalf("accepted omitted structural observation: %v", err)
		}
	})
}

func TestDependencySnapshotRequiresEnabledReferrerObservation(t *testing.T) {
	s := testStore{}
	root := artifact(t, s, "root", nil)
	attachment := artifact(t, s, "attachment", &root)
	m := metadata(root)
	m.Recipe.Include[0].Dependencies.Follow = "auto"
	m.Origins = append(m.Origins, Origin{Repository: m.Roots[0].Repository, Descriptor: attachment})
	m.Referrers = append(m.Referrers, Association{Repository: m.Roots[0].Repository, Subject: root, Descriptor: attachment})
	m.Dependencies = []DependencyResolution{{Repository: m.Roots[0].Repository, Subject: root, Policy: dependency.Policy{Follow: "auto"}, Outcome: "absent", Dependencies: []ResolvedDependency{}}}
	if _, err := BuildSnapshot(context.Background(), s, m); err == nil || !strings.Contains(err.Error(), "missing captured dependency resolution") {
		t.Fatalf("accepted missing enabled referrer observation: %v", err)
	}
	m.Dependencies = append(m.Dependencies, DependencyResolution{Repository: m.Roots[0].Repository, Subject: attachment, Policy: dependency.Policy{Follow: "auto"}, Outcome: "absent", Dependencies: []ResolvedDependency{}})
	if _, err := BuildSnapshot(context.Background(), s, m); err != nil {
		t.Fatal(err)
	}
}

func TestDependencySnapshotMixedPoliciesPreserveOptOut(t *testing.T) {
	s := testStore{}
	root := artifact(t, s, "root", nil)
	attachment := artifact(t, s, "attachment", &root)
	m := metadata(root)
	m.Recipe.Include[0].Dependencies.Follow = "none"
	enabled := m.Recipe.Include[0]
	enabled.Dependencies.Follow = "auto"
	enabled.Referrers.Include = "none"
	m.Recipe.Include = append(m.Recipe.Include, enabled)
	second := m.Roots[0]
	second.Input = 1
	m.Roots = append(m.Roots, second)
	m.Origins = append(m.Origins, Origin{Repository: m.Roots[0].Repository, Descriptor: attachment})
	m.Referrers = append(m.Referrers, Association{Repository: m.Roots[0].Repository, Subject: root, Descriptor: attachment})
	m.Dependencies = []DependencyResolution{
		{Repository: m.Roots[0].Repository, Subject: root, Policy: dependency.Policy{Follow: "none"}, Outcome: "disabled", Dependencies: []ResolvedDependency{}},
		{Repository: m.Roots[0].Repository, Subject: root, Policy: dependency.Policy{Follow: "auto"}, Outcome: "absent", Dependencies: []ResolvedDependency{}},
	}
	if _, err := BuildSnapshot(context.Background(), s, m); err != nil {
		t.Fatalf("mixed policy incorrectly enabled optional attachment traversal: %v", err)
	}
}

func TestDependencySnapshotRejectsClearedDefaultPolicyAndObservations(t *testing.T) {
	s := testStore{}
	root := artifact(t, s, "root", nil)
	m := metadata(root)
	m.Recipe.Include[0].Dependencies.Follow = "auto"
	m.Dependencies = []DependencyResolution{{Repository: m.Roots[0].Repository, Subject: root, Policy: dependency.Policy{Follow: "auto"}, Outcome: "absent", Dependencies: []ResolvedDependency{}}}
	if _, err := BuildSnapshot(context.Background(), s, m); err != nil {
		t.Fatal(err)
	}
	// Empty Follow still means auto. Clearing the field cannot erase the claim
	// that creating this bundle followed the input's dependency declarations.
	m.Recipe.Include[0].Dependencies = dependency.Policy{}
	m.Dependencies = nil
	if _, err := BuildSnapshot(context.Background(), s, m); err == nil || !strings.Contains(err.Error(), "missing captured dependency resolution") {
		t.Fatalf("accepted removal of default-auto observation: %v", err)
	}
}
