package transport

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/deliveryfile"
)

func deliveryUpdateTestMetadata(values ...string) bundle.Metadata {
	m := bundle.Metadata{}
	for _, value := range values {
		d := diffTestDescriptor(value)
		m.Origins = append(m.Origins, bundle.Origin{Repository: "example.test/app", Descriptor: d})
	}
	return m
}

func deliveryUpdateRoot(m *bundle.Metadata, tag, value string) {
	m.Roots = append(m.Roots, bundle.Root{Repository: "example.test/app", Reference: tag, Descriptor: diffTestDescriptor(value)})
}

func deliveryUpdateRef(value string) string {
	return "example.test/app@" + diffTestDescriptor(value).Digest.String()
}

func TestUpdateDeliveryAssignments(t *testing.T) {
	tests := []struct {
		name              string
		old, next         []string
		oldTags, nextTags map[string]string
		paths             map[string]string
		want              map[string]string
		conflict          bool
		changes           map[string]int
	}{
		{name: "empty explicit file defaults everything", old: []string{"a"}, next: []string{"a", "b"}, want: map[string]string{"a": "app", "b": "app"}, changes: map[string]int{"defaulted": 2}},
		{name: "partial file preserves only explicit", old: []string{"a", "b", "removed"}, next: []string{"a", "b", "new"}, paths: map[string]string{"a": "custom/a"}, want: map[string]string{"a": "custom/a", "b": "app", "new": "app"}, changes: map[string]int{"preserved": 1, "defaulted": 2, "removed": 1}},
		{name: "singleton tag moves", old: []string{"a"}, next: []string{"b"}, oldTags: map[string]string{"latest": "a", "alias": "a"}, nextTags: map[string]string{"latest": "b", "alias": "b"}, paths: map[string]string{"a": "custom"}, want: map[string]string{"b": "custom"}, changes: map[string]int{"moved": 1}},
		{name: "removed selector does not infer", old: []string{"a"}, next: []string{"b"}, oldTags: map[string]string{"old": "a"}, nextTags: map[string]string{"new": "b"}, paths: map[string]string{"a": "custom"}, want: map[string]string{"b": "app"}, changes: map[string]int{"removed": 1, "defaulted": 1}},
		{name: "surviving old digest makes fanout", old: []string{"a"}, next: []string{"a", "b"}, oldTags: map[string]string{"latest": "a"}, nextTags: map[string]string{"latest": "b"}, paths: map[string]string{"a": "custom"}, conflict: true},
		{name: "aliases split", old: []string{"a"}, next: []string{"b", "c"}, oldTags: map[string]string{"one": "a", "two": "a"}, nextTags: map[string]string{"one": "b", "two": "c"}, paths: map[string]string{"a": "custom"}, conflict: true},
		{name: "same-path convergence", old: []string{"a", "b"}, next: []string{"c"}, oldTags: map[string]string{"one": "a", "two": "b"}, nextTags: map[string]string{"one": "c", "two": "c"}, paths: map[string]string{"a": "custom", "b": "custom"}, want: map[string]string{"c": "custom"}, changes: map[string]int{"moved": 2}},
		{name: "different-path convergence", old: []string{"a", "b"}, next: []string{"c"}, oldTags: map[string]string{"one": "a", "two": "b"}, nextTags: map[string]string{"one": "c", "two": "c"}, paths: map[string]string{"a": "custom/a", "b": "custom/b"}, conflict: true},
		{name: "exact survivor also claims convergence", old: []string{"a", "b"}, next: []string{"b"}, oldTags: map[string]string{"latest": "a"}, nextTags: map[string]string{"latest": "b"}, paths: map[string]string{"a": "custom/a", "b": "custom/b"}, conflict: true},
		{name: "same-path exact survivor convergence", old: []string{"a", "b"}, next: []string{"b"}, oldTags: map[string]string{"latest": "a"}, nextTags: map[string]string{"latest": "b"}, paths: map[string]string{"a": "custom", "b": "custom"}, want: map[string]string{"b": "custom"}, changes: map[string]int{"preserved": 1, "moved": 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old, next := deliveryUpdateTestMetadata(tt.old...), deliveryUpdateTestMetadata(tt.next...)
			for tag, value := range tt.oldTags {
				deliveryUpdateRoot(&old, tag, value)
			}
			for tag, value := range tt.nextTags {
				deliveryUpdateRoot(&next, tag, value)
			}
			file := deliveryfile.File{Bundle: diffTestDescriptor("old-bundle").Digest, Artifacts: []deliveryfile.Artifact{}}
			for value, path := range tt.paths {
				file.Artifacts = append(file.Artifacts, deliveryfile.Artifact{Ref: deliveryUpdateRef(value), Path: path})
			}
			result, err := updateDelivery(diffTestDescriptor("old-bundle"), diffTestDescriptor("new-bundle"), []bundle.Metadata{old}, []bundle.Metadata{next}, file)
			if tt.conflict {
				if !errors.Is(err, ErrConflict) || result.Delivery != nil || len(result.Conflicts) == 0 {
					t.Fatalf("expected conflict: %#v, %v", result, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{}
			for value, path := range tt.want {
				want[deliveryUpdateRef(value)] = path
			}
			if !reflect.DeepEqual(deliveryArtifacts(*result.Delivery), want) {
				t.Fatalf("paths: %#v", result.Delivery)
			}
			counts := map[string]int{}
			for _, change := range result.Changes {
				counts[change.Change]++
			}
			if !reflect.DeepEqual(counts, tt.changes) {
				t.Fatalf("changes: %#v", result.Changes)
			}
			if result.Delivery.Bundle != diffTestDescriptor("new-bundle").Digest || result.Conflicts == nil {
				t.Fatalf("binding/arrays: %#v", result)
			}
		})
	}
}

func TestUpdateDeliveryAggregatesNestedSelectors(t *testing.T) {
	oldA, oldB := deliveryUpdateTestMetadata("a"), deliveryUpdateTestMetadata("b")
	nextA, nextC := deliveryUpdateTestMetadata("a"), deliveryUpdateTestMetadata("c")
	deliveryUpdateRoot(&oldA, "latest", "a")
	deliveryUpdateRoot(&oldB, "latest", "b")
	deliveryUpdateRoot(&nextA, "latest", "a")
	deliveryUpdateRoot(&nextC, "latest", "c")
	file := deliveryfile.File{Bundle: diffTestDescriptor("old").Digest, Artifacts: []deliveryfile.Artifact{{Ref: deliveryUpdateRef("a"), Path: "custom"}}}
	before, after := diffTestDescriptor("old"), diffTestDescriptor("new")
	result, err := updateDelivery(before, after, []bundle.Metadata{oldA, oldB}, []bundle.Metadata{nextA, nextC}, file)
	if !errors.Is(err, ErrConflict) || len(result.Conflicts) != 1 || result.Conflicts[0].Reason != "selector has multiple captured resolutions" {
		t.Fatalf("nested ambiguity: %#v %v", result, err)
	}
	reversed, err := updateDelivery(before, after, []bundle.Metadata{oldB, oldA}, []bundle.Metadata{nextC, nextA}, file)
	if !errors.Is(err, ErrConflict) || !reflect.DeepEqual(result, reversed) {
		t.Fatalf("ordering changes result: %#v %v", reversed, err)
	}
	result, err = updateDelivery(before, after, []bundle.Metadata{oldA, oldB}, []bundle.Metadata{oldB, oldA}, file)
	if err != nil || result.Delivery.Artifacts[0].Ref == "" {
		t.Fatalf("equal multivalued selectors should preserve exact paths: %#v %v", result, err)
	}
	if deliveryArtifacts(*result.Delivery)[deliveryUpdateRef("a")] != "custom" {
		t.Fatal("explicit exact path lost")
	}
}

func TestUpdateDeliveryValidation(t *testing.T) {
	before, after := diffTestDescriptor("before"), diffTestDescriptor("after")
	m := []bundle.Metadata{deliveryUpdateTestMetadata("a")}
	for _, file := range []deliveryfile.File{
		{Bundle: after.Digest},
		{Bundle: before.Digest, Artifacts: []deliveryfile.Artifact{{Ref: deliveryUpdateRef("unknown"), Path: "custom"}}},
		{Bundle: before.Digest, Artifacts: []deliveryfile.Artifact{{Ref: deliveryUpdateRef("a"), Path: "../invalid"}}},
	} {
		result, err := updateDelivery(before, after, m, m, file)
		if !errors.Is(err, ErrInvalid) || !reflect.DeepEqual(result, DeliveryUpdateResult{}) {
			t.Fatalf("invalid request: %#v %v", result, err)
		}
	}
}

func TestUpdateDeliveryDefensiveCopiesAndDeterminism(t *testing.T) {
	before, after := diffTestDescriptor("before"), diffTestDescriptor("after")
	before.Annotations = map[string]string{"copy": "before"}
	after.URLs = []string{"https://example.test"}
	old, next := deliveryUpdateTestMetadata("a", "b"), deliveryUpdateTestMetadata("c", "d")
	deliveryUpdateRoot(&old, "one", "a")
	deliveryUpdateRoot(&old, "two", "b")
	deliveryUpdateRoot(&next, "one", "c")
	deliveryUpdateRoot(&next, "two", "d")
	file := deliveryfile.File{Bundle: before.Digest, Artifacts: []deliveryfile.Artifact{{Ref: deliveryUpdateRef("b"), Path: "custom/b"}, {Ref: deliveryUpdateRef("a"), Path: "custom/a"}}}
	result, err := updateDelivery(before, after, []bundle.Metadata{old}, []bundle.Metadata{next}, file)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(old.Roots)
	slices.Reverse(next.Origins)
	slices.Reverse(file.Artifacts)
	again, err := updateDelivery(before, after, []bundle.Metadata{old}, []bundle.Metadata{next}, file)
	if err != nil || !reflect.DeepEqual(result, again) {
		t.Fatalf("nondeterministic: %v", err)
	}
	result.Before.Annotations["copy"] = "changed"
	result.After.URLs[0] = "changed"
	result.Delivery.Artifacts[0].Path = "changed"
	if before.Annotations["copy"] != "before" || after.URLs[0] != "https://example.test" || file.Artifacts[0].Path == "changed" {
		t.Fatal("input mutated through report")
	}
	raw, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || again.Changes == nil || again.Conflicts == nil {
		t.Fatal("missing result arrays")
	}
}

func TestUpdateDeliveryRolesAndRepositoriesDoNotPair(t *testing.T) {
	old, next := deliveryUpdateTestMetadata("a"), deliveryUpdateTestMetadata("b")
	deliveryUpdateRoot(&old, "latest", "a")
	next.Plugins = []bundle.PluginResolution{{Ref: "example.test/app:latest", Descriptor: diffTestDescriptor("b")}}
	file := deliveryfile.File{Bundle: diffTestDescriptor("old").Digest, Artifacts: []deliveryfile.Artifact{{Ref: deliveryUpdateRef("a"), Path: "custom"}}}
	result, err := updateDelivery(diffTestDescriptor("old"), diffTestDescriptor("next"), []bundle.Metadata{old}, []bundle.Metadata{next}, file)
	if err != nil || deliveryArtifacts(*result.Delivery)[deliveryUpdateRef("b")] != "app" {
		t.Fatalf("paired unrelated roles: %#v %v", result, err)
	}
	// A new repository with identical content is a distinct inventory identity.
	next.Origins[0] = bundle.Origin{Repository: "elsewhere.test/app", Descriptor: diffTestDescriptor("a")}
	next.Plugins = nil
	result, err = updateDelivery(diffTestDescriptor("old"), diffTestDescriptor("next"), []bundle.Metadata{old}, []bundle.Metadata{next}, file)
	if err != nil || result.Changes[0].Change == "moved" {
		t.Fatalf("paired repositories: %#v %v", result, err)
	}
}

func TestUpdateDeliveryDependencyAndPluginSelectors(t *testing.T) {
	for _, kind := range []string{"dependency", "plugin"} {
		t.Run(kind, func(t *testing.T) {
			old, next := deliveryUpdateTestMetadata("a"), deliveryUpdateTestMetadata("b")
			if kind == "dependency" {
				old.Dependencies = []bundle.DependencyResolution{{Dependencies: []bundle.ResolvedDependency{{Ref: "example.test/app:latest", Descriptor: diffTestDescriptor("a")}}}}
				next.Dependencies = []bundle.DependencyResolution{{Dependencies: []bundle.ResolvedDependency{{Ref: "example.test/app:latest", Descriptor: diffTestDescriptor("b")}}}}
			} else {
				old.Plugins = []bundle.PluginResolution{{Ref: "example.test/app:latest", Descriptor: diffTestDescriptor("a")}}
				next.Plugins = []bundle.PluginResolution{{Ref: "example.test/app:latest", Descriptor: diffTestDescriptor("b")}}
			}
			file := deliveryfile.File{Bundle: diffTestDescriptor("old").Digest, Artifacts: []deliveryfile.Artifact{{Ref: deliveryUpdateRef("a"), Path: "custom"}}}
			result, err := updateDelivery(diffTestDescriptor("old"), diffTestDescriptor("new"), []bundle.Metadata{old}, []bundle.Metadata{next}, file)
			if err != nil || len(result.Changes) != 1 || result.Changes[0].Change != "moved" || deliveryArtifacts(*result.Delivery)[deliveryUpdateRef("b")] != "custom" {
				t.Fatalf("selector not moved: %#v %v", result, err)
			}
		})
	}
}
func TestUpdateDeliveryEmptyInventory(t *testing.T) {
	before, after := diffTestDescriptor("old"), diffTestDescriptor("new")
	result, err := updateDelivery(before, after, nil, nil, deliveryfile.File{Bundle: before.Digest})
	if err != nil || result.Delivery == nil || result.Delivery.Artifacts == nil || result.Changes == nil || result.Conflicts == nil {
		t.Fatalf("empty inventory: %#v %v", result, err)
	}
}

func TestUpdateDeliveryConflictTargetsHaveNoReportedAssignments(t *testing.T) {
	for _, kind := range []string{"fanout", "multivalued", "multivalued-with-exact-survivor"} {
		t.Run(kind, func(t *testing.T) {
			old, next := deliveryUpdateTestMetadata("a"), deliveryUpdateTestMetadata("a", "b", "unrelated")
			deliveryUpdateRoot(&old, "latest", "a")
			deliveryUpdateRoot(&next, "latest", "b")
			if kind == "multivalued" {
				deliveryUpdateRoot(&next, "latest", "a")
			}
			if kind == "multivalued-with-exact-survivor" {
				next.Origins = append(next.Origins, bundle.Origin{Repository: "example.test/app", Descriptor: diffTestDescriptor("c")})
				deliveryUpdateRoot(&next, "latest", "c")
			}
			file := deliveryfile.File{Bundle: diffTestDescriptor("old").Digest, Artifacts: []deliveryfile.Artifact{{Ref: deliveryUpdateRef("a"), Path: "custom"}}}
			result, err := updateDelivery(diffTestDescriptor("old"), diffTestDescriptor("new"), []bundle.Metadata{old}, []bundle.Metadata{next}, file)
			if !errors.Is(err, ErrConflict) || result.Delivery != nil {
				t.Fatalf("expected conflict: %#v %v", result, err)
			}
			for _, conflict := range result.Conflicts {
				for _, target := range conflict.Targets {
					for _, change := range result.Changes {
						if change.After == target {
							t.Fatalf("conflict target has claimed action: %#v", change)
						}
					}
				}
			}
			want := []DeliveryPathChange{{Change: "defaulted", After: deliveryUpdateRef("unrelated"), Path: "app"}}
			if !reflect.DeepEqual(result.Changes, want) {
				t.Fatalf("unrelated defaults lost: %#v", result.Changes)
			}
		})
	}
}
