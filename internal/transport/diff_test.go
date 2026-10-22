package transport

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
)

func diffTestDescriptor(value string) v1.Descriptor {
	return v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString(value), Size: int64(len(value))}
}

func diffTestSnapshot() InspectResult {
	subject, image, declaration, plugin := diffTestDescriptor("subject"), diffTestDescriptor("image"), diffTestDescriptor("declaration"), diffTestDescriptor("plugin")
	policy := dependency.Policy{Follow: "auto"}
	return InspectResult{
		Descriptor: diffTestDescriptor("bundle"),
		Metadata: bundle.Metadata{
			Recipe: config.Recipe{Include: []config.Selection{{Ref: "example.test/charts/web:1", Dependencies: policy}}},
			Roots:  []bundle.Root{{Repository: "example.test/charts/web", Reference: "1", Descriptor: subject}},
			Origins: []bundle.Origin{
				{Repository: "example.test/charts/web", Descriptor: subject},
				{Repository: "example.test/images/web", Descriptor: image},
				{Repository: "example.test/charts/web", Descriptor: declaration},
				{Repository: "example.test/plugins/helm", Descriptor: plugin},
			},
			Dependencies: []bundle.DependencyResolution{{
				Repository: "example.test/charts/web", Subject: subject, Policy: policy,
				Outcome: "selected", Declaration: &declaration,
				Dependencies: []bundle.ResolvedDependency{{Ref: "example.test/images/web:1", Descriptor: image, Policy: policy}},
			}},
			Plugins: []bundle.PluginResolution{{Repository: "example.test/charts/web", Subject: subject,
				Ref: "example.test/plugins/helm:1", Descriptor: plugin, Declaration: declaration}},
		},
		Declarations: []InspectedDeclaration{{Repository: "example.test/charts/web", Subject: subject,
			Descriptor: declaration, Declaration: dependency.Declaration{SchemaVersion: 1, Context: "production"}}},
	}
}

func diffTestCopy(t *testing.T, source InspectResult) InspectResult {
	t.Helper()
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var result InspectResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCompareSnapshotsUnchangedAndReorderedInventory(t *testing.T) {
	before := diffTestSnapshot()
	before.Metadata.Roots = append(before.Metadata.Roots, before.Metadata.Roots[0])
	before.Metadata.Dependencies = append(before.Metadata.Dependencies, before.Metadata.Dependencies[0])
	after := diffTestCopy(t, before)
	slices.Reverse(after.Metadata.Origins)
	slices.Reverse(after.Metadata.Roots)
	slices.Reverse(after.Metadata.Dependencies)
	after.Metadata.Plugins = append(after.Metadata.Plugins, after.Metadata.Plugins[0])
	result := compareSnapshots(before, after)
	if result.Changed || result.RecipeChanged || len(result.Artifacts)+len(result.Resolutions)+len(result.Declarations) != 0 {
		t.Fatalf("unchanged inventories differ: %#v", result)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "null") {
		t.Fatalf("empty changes must encode as arrays: %s", raw)
	}
	// OCI content can differ even if its captured semantic inventory does not.
	after.Descriptor = diffTestDescriptor("different-envelope")
	result = compareSnapshots(before, after)
	if !result.Changed || result.RecipeChanged || len(result.Artifacts)+len(result.Resolutions)+len(result.Declarations) != 0 {
		t.Fatalf("bundle content change: %#v", result)
	}
}

func TestCompareSnapshotsArtifactRepositoryIdentity(t *testing.T) {
	before := diffTestSnapshot()
	after := diffTestCopy(t, before)
	// The same manifest in a different repository is a separate artifact.
	after.Metadata.Origins[1].Repository = "elsewhere.test/images/web"
	// Opaque configs and payloads are not artifacts, even with origin records.
	opaque := diffTestDescriptor("config")
	opaque.MediaType = v1.MediaTypeImageConfig
	after.Metadata.Origins = append(after.Metadata.Origins, bundle.Origin{Repository: "example.test/images/web", Descriptor: opaque})
	result := compareSnapshots(before, after)
	if len(result.Artifacts) != 2 || len(result.Resolutions)+len(result.Declarations) != 0 {
		t.Fatalf("repository movement: %#v", result)
	}
	if result.Artifacts[0].Repository != "elsewhere.test/images/web" || result.Artifacts[0].Change != "added" || result.Artifacts[1].Change != "removed" {
		t.Fatalf("unstable or incorrect artifact changes: %#v", result.Artifacts)
	}
}

func TestCompareSnapshotsTagMovesWithinUnchangedArtifactSet(t *testing.T) {
	before := diffTestSnapshot()
	second := diffTestDescriptor("other-image")
	before.Metadata.Origins = append(before.Metadata.Origins, bundle.Origin{Repository: "example.test/images/web", Descriptor: second})
	after := diffTestCopy(t, before)
	after.Metadata.Dependencies[0].Dependencies[0].Descriptor = second
	result := compareSnapshots(before, after)
	if len(result.Artifacts) != 0 || len(result.Declarations) != 0 || len(result.Resolutions) != 1 {
		t.Fatalf("tag move should only change resolution: %#v", result)
	}
	change := result.Resolutions[0]
	if change.Kind != "dependency" || change.Ref != "example.test/images/web:1" || len(change.Before) != 1 || len(change.After) != 1 || change.After[0].Digest != second.Digest {
		t.Fatalf("unexpected resolution: %#v", change)
	}
}

func TestCompareSnapshotsPluginUpdateWithUnchangedOutput(t *testing.T) {
	before := diffTestSnapshot()
	after := diffTestCopy(t, before)
	after.Metadata.Plugins[0].Descriptor = diffTestDescriptor("plugin-v2")
	result := compareSnapshots(before, after)
	if len(result.Declarations)+len(result.Artifacts) != 0 || len(result.Resolutions) != 1 || result.Resolutions[0].Kind != "plugin" {
		t.Fatalf("plugin update: %#v", result)
	}
}

func TestCompareSnapshotsDescriptorViewsAndSizes(t *testing.T) {
	before := diffTestSnapshot()
	after := diffTestCopy(t, before)
	// A descriptor can carry different edge annotations and platform hints
	// without changing the OCI object it identifies.
	after.Metadata.Roots[0].Descriptor.Annotations = map[string]string{"edge": "new"}
	after.Metadata.Roots[0].Descriptor.Platform = &v1.Platform{OS: "linux", Architecture: "arm64"}
	after.Metadata.Origins[0].Descriptor.Annotations = map[string]string{"edge": "new"}
	after.Metadata.Dependencies[0].Declaration.Annotations = map[string]string{"edge": "new"}
	result := compareSnapshots(before, after)
	if len(result.Artifacts)+len(result.Resolutions)+len(result.Declarations) != 0 {
		t.Fatalf("descriptor views should not change identity: %#v", result)
	}
	// Size is part of descriptor identity. Validated snapshots normally reject
	// inconsistent sizes earlier, but a comparison must not hide them.
	after.Metadata.Roots[0].Descriptor.Size++
	after.Metadata.Dependencies[0].Declaration.Size++
	result = compareSnapshots(before, after)
	if len(result.Resolutions) != 1 || len(result.Declarations) != 1 {
		t.Fatalf("descriptor size change was hidden: %#v", result)
	}
}

func TestCompareSnapshotsDeclarationUpdateWithUnchangedImages(t *testing.T) {
	before := diffTestSnapshot()
	after := diffTestCopy(t, before)
	next := diffTestDescriptor("declaration-v2")
	after.Metadata.Dependencies[0].Declaration = &next
	after.Declarations[0].Descriptor = next
	after.Declarations[0].Declaration.Context = "staging"
	result := compareSnapshots(before, after)
	if len(result.Artifacts)+len(result.Resolutions) != 0 || len(result.Declarations) != 1 {
		t.Fatalf("declaration update: %#v", result)
	}
	change := result.Declarations[0]
	if change.Subject != "example.test/charts/web:1" || len(change.Before) != 1 || len(change.After) != 1 || change.Before[0].Context != "production" || change.After[0].Context != "staging" || change.After[0].Descriptor.Digest != next.Digest {
		t.Fatalf("declaration state lost: %#v", change)
	}
}

func TestCompareSnapshotsPairsTaggedSubjectAcrossDigests(t *testing.T) {
	before := diffTestSnapshot()
	after := diffTestCopy(t, before)
	next := diffTestDescriptor("subject-v2")
	after.Metadata.Roots[0].Descriptor = next
	after.Metadata.Dependencies[0].Subject = next
	after.Declarations[0].Subject = next
	after.Metadata.Dependencies[0].Outcome = "absent"
	after.Metadata.Dependencies[0].Declaration = nil
	result := compareSnapshots(before, after)
	if len(result.Declarations) != 1 || result.Declarations[0].Subject != "example.test/charts/web:1" || len(result.Resolutions) != 1 || result.Resolutions[0].Kind != "input" {
		t.Fatalf("subject update was not paired by selector: %#v", result)
	}
	if result.Declarations[0].After[0].Outcome != "absent" || result.Declarations[0].After[0].Descriptor != nil {
		t.Fatalf("absent declaration state: %#v", result.Declarations)
	}
}

func TestCompareSnapshotsProfilesAndAliasesAreSets(t *testing.T) {
	before := diffTestSnapshot()
	second := before.Metadata.Dependencies[0]
	second.Policy.Select = &dependency.Selector{Context: "staging"}
	secondDeclaration := diffTestDescriptor("staging-declaration")
	second.Declaration = &secondDeclaration
	before.Metadata.Dependencies = append(before.Metadata.Dependencies, second)
	before.Declarations = append(before.Declarations, InspectedDeclaration{
		Repository: second.Repository, Subject: second.Subject, Descriptor: secondDeclaration,
		Declaration: dependency.Declaration{SchemaVersion: 1, Context: "staging"},
	})
	// Both aliases explicitly select both profiles in separate recipe inputs.
	// Input order and duplicate observations must not arbitrarily pair profiles.
	root := before.Metadata.Roots[0]
	for _, reference := range []string{"1", "stable"} {
		for _, policy := range []dependency.Policy{{Follow: "auto"}, second.Policy} {
			if reference == "1" && policy.Select == nil {
				continue
			}
			selection := config.Selection{Ref: root.Repository + ":" + reference, Dependencies: policy}
			input := len(before.Metadata.Recipe.Include)
			before.Metadata.Recipe.Include = append(before.Metadata.Recipe.Include, selection)
			profileRoot := root
			profileRoot.Input, profileRoot.Reference = input, reference
			before.Metadata.Roots = append(before.Metadata.Roots, profileRoot)
		}
	}
	after := diffTestCopy(t, before)
	slices.Reverse(after.Metadata.Roots)
	slices.Reverse(after.Metadata.Dependencies)
	slices.Reverse(after.Declarations)
	after.Metadata.Dependencies = append(after.Metadata.Dependencies, after.Metadata.Dependencies[0])
	result := compareSnapshots(before, after)
	if len(result.Artifacts)+len(result.Resolutions)+len(result.Declarations) != 0 {
		t.Fatalf("equivalent profile sets differ: %#v", result)
	}
	for i := range after.Metadata.Recipe.Include {
		after.Metadata.Recipe.Include[i].Dependencies = dependency.Policy{Follow: "auto"}
	}
	after.Metadata.Dependencies = []bundle.DependencyResolution{before.Metadata.Dependencies[0]}
	result = compareSnapshots(before, after)
	if len(result.Declarations) != 2 {
		t.Fatalf("both aliases should report removed profile: %#v", result)
	}
	for _, change := range result.Declarations {
		if len(change.Before) != 2 || len(change.After) != 1 {
			t.Fatalf("profiles were not kept as sets: %#v", change)
		}
	}
}

func TestCompareSnapshotsKeepsAliasPolicyAssociation(t *testing.T) {
	before := diffTestSnapshot()
	production := dependency.Policy{Follow: "auto", Select: &dependency.Selector{Context: "production"}}
	staging := dependency.Policy{Follow: "auto", Select: &dependency.Selector{Context: "staging"}}
	before.Metadata.Recipe.Include[0].Ref = "example.test/charts/web:prod"
	before.Metadata.Recipe.Include[0].Dependencies = production
	before.Metadata.Recipe.Include = append(before.Metadata.Recipe.Include,
		config.Selection{Ref: "example.test/charts/web:stage", Dependencies: staging})
	before.Metadata.Roots[0].Reference = "prod"
	alias := before.Metadata.Roots[0]
	alias.Input, alias.Reference = 1, "stage"
	before.Metadata.Roots = append(before.Metadata.Roots, alias)
	before.Metadata.Dependencies[0].Policy = production
	stageObservation := before.Metadata.Dependencies[0]
	stageObservation.Policy = staging
	stageDeclaration := diffTestDescriptor("stage-declaration")
	stageObservation.Declaration = &stageDeclaration
	before.Metadata.Dependencies = append(before.Metadata.Dependencies, stageObservation)
	before.Declarations = append(before.Declarations, InspectedDeclaration{
		Repository: stageObservation.Repository, Subject: stageObservation.Subject, Descriptor: stageDeclaration,
		Declaration: dependency.Declaration{SchemaVersion: 1, Context: "staging"},
	})
	after := diffTestCopy(t, before)
	// Both tags still pin the same subject and the same two observations exist,
	// but the recipe has swapped the policies attached to each selector.
	after.Metadata.Recipe.Include[0].Dependencies = staging
	after.Metadata.Recipe.Include[1].Dependencies = production
	result := compareSnapshots(before, after)
	if len(result.Declarations) != 2 || len(result.Artifacts)+len(result.Resolutions) != 0 {
		t.Fatalf("selector policy swap was hidden: %#v", result)
	}
	for _, change := range result.Declarations {
		if len(change.Before) != 1 || len(change.After) != 1 || change.Before[0].Context == change.After[0].Context {
			t.Fatalf("unrelated profiles broadcast across aliases: %#v", change)
		}
	}
	if result.Declarations[0].Subject != "example.test/charts/web:prod" || result.Declarations[0].Before[0].Context != "production" || result.Declarations[0].After[0].Context != "staging" || result.Declarations[1].Subject != "example.test/charts/web:stage" {
		t.Fatalf("incorrect policy associations: %#v", result.Declarations)
	}

	t.Run("plugin-generated policies", func(t *testing.T) {
		for i := range before.Metadata.Recipe.Include {
			before.Metadata.Recipe.Include[i].Dependencies = dependency.Policy{Follow: "auto"}
			before.Metadata.Recipe.Include[i].Plugin = &config.Plugin{Ref: "example.test/plugins/helm:1", Input: json.RawMessage(`{}`)}
			before.Metadata.Dependencies[i].Policy = dependency.Policy{Follow: "auto", Select: &dependency.Selector{Digest: before.Metadata.Dependencies[i].Declaration.Digest}}
		}
		stagePlugin := before.Metadata.Plugins[0]
		stagePlugin.Input, stagePlugin.Declaration = 1, stageDeclaration
		before.Metadata.Plugins = append(before.Metadata.Plugins, stagePlugin)
		after := diffTestCopy(t, before)
		after.Metadata.Plugins[0].Declaration, after.Metadata.Plugins[1].Declaration = after.Metadata.Plugins[1].Declaration, after.Metadata.Plugins[0].Declaration
		result := compareSnapshots(before, after)
		if len(result.Declarations) != 2 {
			t.Fatalf("generated plugin policies were not associated with their roots: %#v", result)
		}
		for _, change := range result.Declarations {
			if len(change.Before) != 1 || len(change.After) != 1 || change.Before[0].Context == change.After[0].Context {
				t.Fatalf("generated profiles broadcast across aliases: %#v", change)
			}
		}
	})
}

func TestCompareSnapshotsDeclarationFallbackAndDependencySelectors(t *testing.T) {
	before := diffTestSnapshot()
	image := before.Metadata.Dependencies[0].Dependencies[0]
	observation := bundle.DependencyResolution{Repository: "example.test/images/web", Subject: image.Descriptor,
		Policy: dependency.Policy{Follow: "auto"}, Outcome: "absent"}
	before.Metadata.Dependencies = append(before.Metadata.Dependencies, observation)
	unknown := bundle.DependencyResolution{Repository: "example.test/opaque", Subject: diffTestDescriptor("opaque"),
		Policy: dependency.Policy{Follow: "auto"}, Outcome: "absent"}
	before.Metadata.Dependencies = append(before.Metadata.Dependencies, unknown)
	after := diffTestCopy(t, before)
	for i := 1; i < len(after.Metadata.Dependencies); i++ {
		after.Metadata.Dependencies[i].Policy.Follow = "none"
		after.Metadata.Dependencies[i].Outcome = "disabled"
	}
	after.Metadata.Dependencies[0].Dependencies[0].Policy.Follow = "none"
	result := compareSnapshots(before, after)
	if len(result.Declarations) != 2 || result.Declarations[0].Subject != image.Ref || result.Declarations[1].Subject != unknown.Repository+"@"+unknown.Subject.Digest.String() {
		t.Fatalf("subject selector/fallback: %#v", result)
	}
}

func TestCompareSnapshotsRecipeSemantics(t *testing.T) {
	before := diffTestSnapshot()
	before.Metadata.Recipe.Include[0].Plugin = &config.Plugin{Ref: "example.test/plugins/helm:1",
		Input: json.RawMessage(`{"large":9007199254740993,"nested":{"b":null,"a":[1,2]}}`)}
	before.Metadata.Dependencies[0].Policy, _ = bundle.RootDependencyPolicy(before.Metadata, before.Metadata.Roots[0])
	after := diffTestCopy(t, before)
	after.Metadata.Recipe.Include[0].Plugin.Input = json.RawMessage(`{ "nested": {"a":[1,2], "b":null}, "large":9007199254740993 }`)
	if compareSnapshots(before, after).RecipeChanged {
		t.Fatal("object ordering/whitespace is not a recipe change")
	}
	for _, input := range []string{
		`{"large":9007199254740992,"nested":{"b":null,"a":[1,2]}}`,
		`{"large":9007199254740993,"nested":{"a":[1,2]}}`,
		`{"large":9007199254740993,"nested":{"b":null,"a":[2,1]}}`,
	} {
		after.Metadata.Recipe.Include[0].Plugin.Input = json.RawMessage(input)
		if !compareSnapshots(before, after).RecipeChanged {
			t.Fatalf("recipe change lost: %s", input)
		}
	}
	before.Metadata.Recipe.Include = append(before.Metadata.Recipe.Include, config.Selection{Ref: "example.test/other:1"})
	after = diffTestCopy(t, before)
	slices.Reverse(after.Metadata.Recipe.Include)
	after.Metadata.Roots[0].Input = 1
	after.Metadata.Plugins[0].Input = 1
	result := compareSnapshots(before, after)
	if !result.RecipeChanged || len(result.Artifacts)+len(result.Resolutions)+len(result.Declarations) != 0 {
		t.Fatalf("recipe order should be isolated: %#v", result)
	}
}

func TestCompareSnapshotsResolutionSetsSeparateKindsAndProfiles(t *testing.T) {
	before := diffTestSnapshot()
	ref := before.Metadata.Roots[0].Repository + ":1"
	before.Metadata.Plugins[0].Ref = ref
	before.Metadata.Dependencies[0].Dependencies[0].Ref = ref
	after := diffTestCopy(t, before)
	second := diffTestDescriptor("second-profile-resolution")
	profile := after.Metadata.Dependencies[0]
	profile.Dependencies = slices.Clone(profile.Dependencies)
	profile.Dependencies[0].Descriptor = second
	after.Metadata.Dependencies = append(after.Metadata.Dependencies, profile)
	after.Metadata.Plugins[0].Descriptor = second
	after.Metadata.Roots[0].Descriptor = second
	result := compareSnapshots(before, after)
	if len(result.Resolutions) != 3 {
		t.Fatalf("resolution kinds were merged: %#v", result.Resolutions)
	}
	if result.Resolutions[0].Kind != "dependency" || len(result.Resolutions[0].After) != 2 || result.Resolutions[1].Kind != "input" || result.Resolutions[2].Kind != "plugin" {
		t.Fatalf("incorrect resolution sets/order: %#v", result.Resolutions)
	}
}

func TestCompareSnapshotsResultOwnsDescriptorAndPolicyData(t *testing.T) {
	before := diffTestSnapshot()
	enrich := func(descriptor *v1.Descriptor) {
		descriptor.Annotations = map[string]string{"key": "original"}
		descriptor.URLs = []string{"original"}
		descriptor.Data = []byte("original")
		descriptor.Platform = &v1.Platform{OSFeatures: []string{"original"}}
	}
	enrich(&before.Descriptor)
	enrich(&before.Metadata.Origins[0].Descriptor)
	enrich(&before.Metadata.Roots[0].Descriptor)
	enrich(before.Metadata.Dependencies[0].Declaration)
	before.Metadata.Dependencies[0].Policy.Select = &dependency.Selector{Context: "original"}
	beforeOriginal := diffTestCopy(t, before)
	after := InspectResult{Descriptor: diffTestDescriptor("empty"), Metadata: bundle.Metadata{}}
	result := compareSnapshots(before, after)
	mutate := func(descriptor *v1.Descriptor) {
		descriptor.Annotations["key"] = "mutated"
		descriptor.URLs[0] = "mutated"
		descriptor.Data[0] = '!'
		descriptor.Platform.OSFeatures[0] = "mutated"
	}
	mutate(&result.Before)
	for i := range result.Artifacts {
		if result.Artifacts[i].Descriptor.Annotations != nil {
			mutate(&result.Artifacts[i].Descriptor)
		}
	}
	for i := range result.Resolutions {
		for j := range result.Resolutions[i].Before {
			if result.Resolutions[i].Before[j].Annotations != nil {
				mutate(&result.Resolutions[i].Before[j])
			}
		}
	}
	state := &result.Declarations[0].Before[0]
	mutate(state.Descriptor)
	state.Policy.Select.Context = "mutated"
	if !reflect.DeepEqual(before, beforeOriginal) {
		t.Fatal("diff result aliases inspected metadata")
	}
	// Repeated calls and aliases also receive independently owned values.
	if !reflect.DeepEqual(compareSnapshots(before, after), compareSnapshots(beforeOriginal, after)) {
		t.Fatal("result mutations changed subsequent comparisons")
	}
}
