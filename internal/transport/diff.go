package transport

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strconv"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/registry"
)

// DiffResult compares the captured inventories of two inspected bundles. Changes
// belong to the root bundle; nested bundles can be compared separately.
type DiffResult struct {
	Before        v1.Descriptor       `json:"before"`
	After         v1.Descriptor       `json:"after"`
	Changed       bool                `json:"changed"`
	RecipeChanged bool                `json:"recipeChanged"`
	Artifacts     []ArtifactChange    `json:"artifacts"`
	Resolutions   []ResolutionChange  `json:"resolutions"`
	Declarations  []DeclarationChange `json:"declarations"`
}

// ArtifactChange identifies a manifest or index added to or removed from a
// source repository. Configs and payload blobs are not artifacts.
type ArtifactChange struct {
	Change     string        `json:"change"`
	Repository string        `json:"repository"`
	Descriptor v1.Descriptor `json:"descriptor"`
}

// ResolutionChange compares the descriptor sets pinned by an original input,
// dependency, or plugin selector. Repeated aliases and profiles form one set.
type ResolutionChange struct {
	Kind   string          `json:"kind"`
	Ref    string          `json:"ref"`
	Before []v1.Descriptor `json:"before"`
	After  []v1.Descriptor `json:"after"`
}

// DeclarationState records an observed dependency policy and its outcome.
type DeclarationState struct {
	Policy     dependency.Policy `json:"policy"`
	Outcome    string            `json:"outcome"`
	Descriptor *v1.Descriptor    `json:"descriptor,omitempty"`
	Context    string            `json:"context,omitempty"`
}

// DeclarationChange compares observation sets for a stable subject selector.
// Subjects without a captured selector use repository@digest instead.
type DeclarationChange struct {
	Subject string             `json:"subject"`
	Before  []DeclarationState `json:"before"`
	After   []DeclarationState `json:"after"`
}

func compareSnapshots(before, after InspectResult) DiffResult {
	result := DiffResult{
		Before: cloneDescriptor(before.Descriptor), After: cloneDescriptor(after.Descriptor),
		Changed:       before.Descriptor.Digest != after.Descriptor.Digest,
		RecipeChanged: !sameDiffRecipe(before.Metadata.Recipe, after.Metadata.Recipe),
		Artifacts:     []ArtifactChange{}, Resolutions: []ResolutionChange{}, Declarations: []DeclarationChange{},
	}
	oldArtifacts, newArtifacts := diffArtifacts(before.Metadata), diffArtifacts(after.Metadata)
	for _, key := range diffKeys(oldArtifacts, newArtifacts) {
		old, had := oldArtifacts[key]
		next, has := newArtifacts[key]
		if had == has {
			continue
		}
		change := next
		change.Change = "added"
		if had {
			change = old
			change.Change = "removed"
		}
		change.Descriptor = cloneDescriptor(change.Descriptor)
		result.Artifacts = append(result.Artifacts, change)
	}
	oldResolutions, newResolutions := diffResolutions(before.Metadata), diffResolutions(after.Metadata)
	for _, key := range diffResolutionKeys(oldResolutions, newResolutions) {
		old, next := oldResolutions[key], newResolutions[key]
		if slices.Equal(diffKeys(old, nil), diffKeys(next, nil)) {
			continue
		}
		result.Resolutions = append(result.Resolutions, ResolutionChange{
			Kind: key.kind, Ref: key.ref, Before: diffDescriptorList(old), After: diffDescriptorList(next),
		})
	}
	oldDeclarations, newDeclarations := diffDeclarations(before), diffDeclarations(after)
	for _, key := range diffKeys(oldDeclarations, newDeclarations) {
		old, next := oldDeclarations[key], newDeclarations[key]
		if slices.Equal(diffKeys(old, nil), diffKeys(next, nil)) {
			continue
		}
		result.Declarations = append(result.Declarations, DeclarationChange{
			Subject: key, Before: diffStateList(old), After: diffStateList(next),
		})
	}
	return result
}

func diffArtifacts(metadata bundle.Metadata) map[string]ArtifactChange {
	result := map[string]ArtifactChange{}
	for _, origin := range metadata.Origins {
		if !bundle.IsEnvelope(origin.Descriptor.MediaType) {
			continue
		}
		key := origin.Repository + "@" + origin.Descriptor.Digest.String()
		old, exists := result[key]
		if !exists || diffJSON(origin.Descriptor) < diffJSON(old.Descriptor) {
			result[key] = ArtifactChange{Repository: origin.Repository, Descriptor: origin.Descriptor}
		}
	}
	return result
}

type diffResolutionKey struct{ kind, ref string }
type diffDescriptorSet map[string]v1.Descriptor

func diffResolutions(metadata bundle.Metadata) map[diffResolutionKey]diffDescriptorSet {
	result := map[diffResolutionKey]diffDescriptorSet{}
	add := func(kind, ref string, descriptor v1.Descriptor) {
		key := diffResolutionKey{kind, ref}
		if result[key] == nil {
			result[key] = diffDescriptorSet{}
		}
		identity := descriptor.Digest.String() + "/" + strconv.FormatInt(descriptor.Size, 10)
		old, exists := result[key][identity]
		if !exists || diffJSON(descriptor) < diffJSON(old) {
			result[key][identity] = descriptor
		}
	}
	for _, root := range metadata.Roots {
		add("input", diffRootRef(root), root.Descriptor)
	}
	for _, observation := range metadata.Dependencies {
		for _, target := range observation.Dependencies {
			add("dependency", target.Ref, target.Descriptor)
		}
	}
	for _, plugin := range metadata.Plugins {
		add("plugin", plugin.Ref, plugin.Descriptor)
	}
	return result
}

func diffRootRef(root bundle.Root) string {
	ref, _ := registry.ParseReference(root.Repository)
	ref.Reference = root.Reference
	return ref.String()
}

type diffStateSet map[string]DeclarationState

func diffDeclarations(inspected InspectResult) map[string]diffStateSet {
	// A subject can have multiple selectors and policies. Associate each incoming
	// selector only with its effective policy, so aliases resolving to the same
	// content do not inherit each other's declaration selections.
	selectors := map[string]map[string]bool{}
	addSelector := func(repository string, descriptor v1.Descriptor, policy dependency.Policy, selector string) {
		key := diffObservationKey(repository, descriptor, policy)
		if selectors[key] == nil {
			selectors[key] = map[string]bool{}
		}
		selectors[key][selector] = true
	}
	for _, root := range inspected.Metadata.Roots {
		policy, _ := bundle.RootDependencyPolicy(inspected.Metadata, root) // Verified during inspection.
		addSelector(root.Repository, root.Descriptor, policy, diffRootRef(root))
	}
	for _, observation := range inspected.Metadata.Dependencies {
		for _, target := range observation.Dependencies {
			ref, _ := registry.ParseReference(target.Ref)
			addSelector(repositoryName(ref), target.Descriptor, target.Policy, target.Ref)
		}
	}
	contexts := map[string]string{}
	for _, declaration := range inspected.Declarations {
		key := declaration.Repository + "@" + declaration.Subject.Digest.String() + "@" + declaration.Descriptor.Digest.String()
		contexts[key] = declaration.Declaration.Context
	}
	result := map[string]diffStateSet{}
	for _, observation := range inspected.Metadata.Dependencies {
		key := observation.Repository + "@" + observation.Subject.Digest.String()
		state := DeclarationState{Policy: observation.Policy, Outcome: observation.Outcome, Descriptor: observation.Declaration}
		if state.Descriptor != nil {
			state.Context = contexts[key+"@"+state.Descriptor.Digest.String()]
		}
		identity := diffStateIdentity(state)
		subjects := selectors[diffObservationKey(observation.Repository, observation.Subject, observation.Policy)]
		if len(subjects) == 0 {
			subjects = map[string]bool{key: true}
		}
		for subject := range subjects {
			if result[subject] == nil {
				result[subject] = diffStateSet{}
			}
			old, exists := result[subject][identity]
			if !exists || diffJSON(state) < diffJSON(old) {
				result[subject][identity] = state
			}
		}
	}
	return result
}

func diffObservationKey(repository string, descriptor v1.Descriptor, policy dependency.Policy) string {
	policy, _ = dependency.NormalizePolicy(policy) // Inspected policies are valid.
	return repository + "@" + descriptor.Digest.String() + "/" + diffJSON(policy)
}

func diffStateIdentity(state DeclarationState) string {
	// Descriptor annotations and transport hints do not change content identity.
	if state.Descriptor != nil {
		state.Descriptor = &v1.Descriptor{Digest: state.Descriptor.Digest, Size: state.Descriptor.Size}
	}
	return diffJSON(state)
}

func diffStateList(states diffStateSet) []DeclarationState {
	result := make([]DeclarationState, 0, len(states))
	for _, key := range diffKeys(states, nil) {
		state := states[key]
		if state.Policy.Select != nil {
			selector := *state.Policy.Select
			state.Policy.Select = &selector
		}
		if state.Descriptor != nil {
			descriptor := cloneDescriptor(*state.Descriptor)
			state.Descriptor = &descriptor
		}
		result = append(result, state)
	}
	return result
}

func diffDescriptorList(descriptors diffDescriptorSet) []v1.Descriptor {
	result := make([]v1.Descriptor, 0, len(descriptors))
	for _, key := range diffKeys(descriptors, nil) {
		result = append(result, cloneDescriptor(descriptors[key]))
	}
	return result
}

// JSON round-tripping normalizes raw plugin input object ordering without
// rounding numbers through float64. Array order and explicit nulls remain real
// recipe differences.
func sameDiffRecipe(before, after any) bool {
	decode := func(value any) any {
		decoder := json.NewDecoder(bytes.NewBufferString(diffJSON(value)))
		decoder.UseNumber()
		var result any
		_ = decoder.Decode(&result) // Inspected recipes are valid JSON values.
		return result
	}
	return reflect.DeepEqual(decode(before), decode(after))
}

func diffJSON(value any) string {
	raw, _ := json.Marshal(value) // Values originate from verified metadata.
	return string(raw)
}

func diffKeys[V any](before, after map[string]V) []string {
	keys := make(map[string]bool, len(before)+len(after))
	for key := range before {
		keys[key] = true
	}
	for key := range after {
		keys[key] = true
	}
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	slices.Sort(result)
	return result
}

func diffResolutionKeys(before, after map[diffResolutionKey]diffDescriptorSet) []diffResolutionKey {
	keys := make(map[diffResolutionKey]bool, len(before)+len(after))
	for key := range before {
		keys[key] = true
	}
	for key := range after {
		keys[key] = true
	}
	result := make([]diffResolutionKey, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].kind != result[j].kind {
			return result[i].kind < result[j].kind
		}
		return result[i].ref < result[j].ref
	})
	return result
}
