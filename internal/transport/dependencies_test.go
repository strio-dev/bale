package transport

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
)

func dependencyDeclaration(t *testing.T, r *readRegistry, subject v1.Descriptor, context string, refs ...dependency.Reference) v1.Descriptor {
	t.Helper()
	raw, err := dependency.Encode(dependency.Declaration{Context: context, Dependencies: refs})
	if err != nil {
		t.Fatal(err)
	}
	return dependencyDeclarationBytes(t, r, subject, dependency.ArtifactType, raw)
}
func dependencyDeclarationBytes(t *testing.T, r *readRegistry, subject v1.Descriptor, artifactType string, raw []byte) v1.Descriptor {
	t.Helper()
	cfg := put(t, r.store, v1.MediaTypeEmptyJSON, []byte("{}"))
	layer := put(t, r.store, artifactType, raw)
	manifest, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: artifactType, Subject: &subject, Config: cfg, Layers: []v1.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	d := put(t, r.store, v1.MediaTypeImageManifest, manifest)
	d.ArtifactType = artifactType
	r.refs[subject.Digest.String()] = append(r.refs[subject.Digest.String()], d)
	return d
}
func dependencySelection(ref string) config.Selection {
	return config.Selection{Ref: ref, Referrers: policy("none")}
}
func capturedOrigin(m bundle.Metadata, d v1.Descriptor) bool {
	for _, o := range m.Origins {
		if o.Descriptor.Digest == d.Digest {
			return true
		}
	}
	return false
}

func TestDependencyCaptureCycleTagsAndDeduplication(t *testing.T) {
	r, repo, s := testRegistry(t)
	a := object(t, r.store, "component A")
	b := object(t, r.store, "component B")
	r.tags["a"] = a
	r.tags["b"] = b
	r.tags["alias"] = b
	da := dependencyDeclaration(t, r, a, "", dependency.Reference{Ref: repo + ":b"}, dependency.Reference{Ref: repo + ":b"}, dependency.Reference{Ref: repo + ":alias"})
	db := dependencyDeclaration(t, r, b, "", dependency.Reference{Ref: repo + ":a"})
	_, snap, err := s.capture(ctx, config.Recipe{Include: []config.Selection{dependencySelection(repo + ":a")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Metadata.Dependencies) != 2 || len(snap.Metadata.Referrers) != 2 {
		t.Fatalf("lost cycle capture: %+v", snap.Metadata)
	}
	for _, tag := range []string{"a", "b", "alias"} {
		if r.headTags[tag] != 1 {
			t.Errorf("%s resolved %d times", tag, r.headTags[tag])
		}
	}
	for _, d := range []v1.Descriptor{a, b, da, db} {
		if !capturedOrigin(snap.Metadata, d) {
			t.Errorf("missing required origin %s", d.Digest)
		}
	}
	// Build against the complete source store to independently verify captured
	// membership and prove the cycle does not become an OCI content-hash cycle.
	locked, err := bundle.BuildSnapshot(ctx, r.store, snap.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.LoadSnapshotGraph(ctx, r.store, locked.Descriptor); err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	raw, err := r.store.Fetch(ctx, locked.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := json.NewDecoder(raw).Decode(&index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 5 {
		t.Fatalf("want metadata plus four unique artifacts, got %d", len(index.Manifests))
	}
}

func TestDependencyCaptureOptOutAndIndependentEnablement(t *testing.T) {
	r, repo, s := testRegistry(t)
	a := object(t, r.store, "root")
	b := object(t, r.store, "direct dependency")
	c := object(t, r.store, "transitive dependency")
	r.tags["a"] = a
	r.tags["b"] = b
	r.tags["c"] = c
	dependencyDeclaration(t, r, a, "", dependency.Reference{Ref: repo + ":b", Follow: "none"})
	dependencyDeclaration(t, r, b, "", dependency.Reference{Ref: repo + ":c"})
	first := dependencySelection(repo + ":a")
	_, snap, err := s.capture(ctx, config.Recipe{Include: []config.Selection{first}})
	if err != nil {
		t.Fatal(err)
	}
	if !capturedOrigin(snap.Metadata, b) || capturedOrigin(snap.Metadata, c) {
		t.Fatal("entry opt-out failed to stop semantic expansion")
	}
	_, snap, err = s.capture(ctx, config.Recipe{Include: []config.Selection{first, dependencySelection(repo + ":b")}})
	if err != nil {
		t.Fatal(err)
	}
	if !capturedOrigin(snap.Metadata, c) {
		t.Fatal("independently enabled root did not expand")
	}
	disabled := dependencySelection(repo + ":a")
	disabled.Dependencies.Follow = "none"
	_, snap, err = s.capture(ctx, config.Recipe{Include: []config.Selection{disabled}})
	if err != nil {
		t.Fatal(err)
	}
	if capturedOrigin(snap.Metadata, b) || len(snap.Metadata.Referrers) != 0 {
		t.Fatal("root opt-out expanded declarations")
	}
}

func TestDependencyCaptureDeclarationSelection(t *testing.T) {
	r, repo, s := testRegistry(t)
	root := object(t, r.store, "chart")
	dev := object(t, r.store, "dev")
	prod := object(t, r.store, "prod")
	r.tags["root"] = root
	r.tags["dev"] = dev
	r.tags["prod"] = prod
	dependencyDeclaration(t, r, root, "dev", dependency.Reference{Ref: repo + ":dev"})
	selected := dependencyDeclaration(t, r, root, "prod", dependency.Reference{Ref: repo + ":prod"})
	cases := []struct {
		name     string
		selector *dependency.Selector
		want     error
	}{
		{"ambiguous", nil, ErrConflict},
		{"context", &dependency.Selector{Context: "prod"}, nil},
		{"digest", &dependency.Selector{Digest: selected.Digest}, nil},
		{"missing context", &dependency.Selector{Context: "missing"}, ErrNotFound},
		{"missing digest", &dependency.Selector{Digest: root.Digest}, ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := dependencySelection(repo + ":root")
			input.Dependencies.Select = tc.selector
			_, snap, err := s.capture(ctx, config.Recipe{Include: []config.Selection{input}})
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("want %v, got %v", tc.want, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !capturedOrigin(snap.Metadata, prod) || capturedOrigin(snap.Metadata, dev) || len(snap.Metadata.Referrers) != 1 {
				t.Fatal("selection included wrong declaration or graph")
			}
		})
	}
}

func TestDependencyCaptureRejectsInvalidCandidatesBeforeSelection(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			r, repo, s := testRegistry(t)
			root := object(t, r.store, "root")
			r.tags["root"] = root
			dependencyDeclaration(t, r, root, "production")
			badType := "application/vnd.bale.dependencies." + version + "+json"
			dependencyDeclarationBytes(t, r, root, badType, []byte(`{"schemaVersion":1,"context":"other","dependencies":[{"ref":"bad"}]}`))
			input := dependencySelection(repo + ":root")
			input.Dependencies.Select = &dependency.Selector{Context: "production"}
			_, _, err := s.capture(ctx, config.Recipe{Include: []config.Selection{input}})
			want := ErrIntegrity
			if version == "v2" {
				want = ErrUnsupported
			}
			if !errors.Is(err, want) {
				t.Fatalf("wanted %v, got %v", want, err)
			}
		})
	}
}

func TestDependencyCaptureKeepsRepositoryScopedDeclarations(t *testing.T) {
	left, leftRepo, _ := testRegistry(t)
	right, rightRepo, _ := testRegistry(t)
	a := object(t, left.store, "same root")
	same := object(t, right.store, "same root")
	if a.Digest != same.Digest {
		t.Fatal("fixture roots differ")
	}
	b := object(t, left.store, "left dependency")
	c := object(t, right.store, "right dependency")
	left.tags["root"] = a
	right.tags["root"] = same
	left.tags["child"] = b
	right.tags["child"] = c
	dependencyDeclaration(t, left, a, "", dependency.Reference{Ref: leftRepo + ":child"})
	dependencyDeclaration(t, right, same, "", dependency.Reference{Ref: rightRepo + ":child"})
	hosts := []string{strings.SplitN(leftRepo, "/", 2)[0], strings.SplitN(rightRepo, "/", 2)[0]}
	s, err := NewService(Options{PlainHTTPRegistries: hosts})
	if err != nil {
		t.Fatal(err)
	}
	_, snap, err := s.capture(ctx, config.Recipe{Include: []config.Selection{dependencySelection(leftRepo + ":root"), dependencySelection(rightRepo + ":root")}})
	if err != nil {
		t.Fatal(err)
	}
	if !capturedOrigin(snap.Metadata, b) || !capturedOrigin(snap.Metadata, c) || len(snap.Metadata.Referrers) != 2 {
		t.Fatal("content dedupe erased repository-scoped dependencies")
	}
}

func TestDependencyCapturePreservesMultiarchAndNestedBundleLock(t *testing.T) {
	r, repo, s := testRegistry(t)
	chart := object(t, r.store, "chart")
	amd := object(t, r.store, "amd64")
	arm := object(t, r.store, "arm64")
	amd.Platform = &v1.Platform{OS: "linux", Architecture: "amd64"}
	arm.Platform = &v1.Platform{OS: "linux", Architecture: "arm64"}
	raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{amd, arm}})
	if err != nil {
		t.Fatal(err)
	}
	image := put(t, r.store, v1.MediaTypeImageIndex, raw)
	r.tags["chart"] = chart
	r.tags["image"] = image
	dependencyDeclaration(t, r, chart, "", dependency.Reference{Ref: repo + ":image"})
	recipe := config.Recipe{Include: []config.Selection{dependencySelection(repo + ":chart")}}
	_, captured, err := s.capture(ctx, recipe)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []v1.Descriptor{image, amd, arm} {
		if !capturedOrigin(captured.Metadata, d) {
			t.Fatalf("lost multiarch member %s", d.Digest)
		}
	}
	inner, err := bundle.BuildSnapshot(ctx, r.store, captured.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	r.tags["bundle"] = inner.Descriptor
	// A later mutable tag and declaration must affect a fresh chart capture,
	// while selecting an existing native bundle keeps its earlier graph locked.
	replacement := object(t, r.store, "replacement image")
	r.tags["image"] = replacement
	dependencyDeclaration(t, r, image, "", dependency.Reference{Ref: repo + ":never-resolve"})
	nestedRecipe := config.Recipe{Include: []config.Selection{dependencySelection(repo + ":bundle")}}
	_, outer, err := s.capture(ctx, nestedRecipe)
	if err != nil {
		t.Fatal(err)
	}
	if !capturedOrigin(outer.Metadata, image) || !capturedOrigin(outer.Metadata, amd) || capturedOrigin(outer.Metadata, replacement) {
		t.Fatal("nested bundle capture changed locked members")
	}
	_, fresh, err := s.capture(ctx, recipe)
	if err != nil {
		t.Fatal(err)
	}
	if !capturedOrigin(fresh.Metadata, replacement) || capturedOrigin(fresh.Metadata, image) {
		t.Fatal("fresh capture did not refresh source tag")
	}
}
