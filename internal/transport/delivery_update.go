package transport

import (
	"fmt"
	"slices"
	"sort"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/deliveryfile"
	"oras.land/oras-go/v2/registry"
)

// DeliveryUpdateResult reports the path assignments carried into a refreshed
// bundle. Delivery is absent when any assignment is ambiguous.
type DeliveryUpdateResult struct {
	Before    v1.Descriptor          `json:"before"`
	After     v1.Descriptor          `json:"after"`
	Delivery  *deliveryfile.File     `json:"delivery,omitempty"`
	Changes   []DeliveryPathChange   `json:"changes"`
	Conflicts []DeliveryPathConflict `json:"conflicts"`
}

// DeliveryPathChange identifies a preserved, moved, defaulted, or removed path.
type DeliveryPathChange struct {
	Change string `json:"change"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	Path   string `json:"path,omitempty"`
}

// DeliveryPathConflict records the explicit assignments and selector evidence
// that prevent a safe update.
type DeliveryPathConflict struct {
	Reason    string                  `json:"reason"`
	Sources   []deliveryfile.Artifact `json:"sources"`
	Targets   []string                `json:"targets"`
	Selectors []string                `json:"selectors"`
}

func updateDelivery(before, after v1.Descriptor, beforeMetadata, afterMetadata []bundle.Metadata, file deliveryfile.File) (DeliveryUpdateResult, error) {
	result := DeliveryUpdateResult{Before: cloneDescriptor(before), After: cloneDescriptor(after), Changes: []DeliveryPathChange{}, Conflicts: []DeliveryPathConflict{}}
	normalized, err := deliveryfile.Normalize(file)
	if err != nil {
		return DeliveryUpdateResult{}, fmt.Errorf("%w: delivery file: %v", ErrInvalid, err)
	}
	if normalized.Bundle != before.Digest {
		return DeliveryUpdateResult{}, fmt.Errorf("%w: delivery file is bound to %s, expected %s", ErrInvalid, normalized.Bundle, before.Digest)
	}
	oldFile, err := defaultDelivery(before, beforeMetadata)
	if err != nil {
		return DeliveryUpdateResult{}, err
	}
	nextFile, err := defaultDelivery(after, afterMetadata)
	if err != nil {
		return DeliveryUpdateResult{}, err
	}
	old, next := deliveryArtifacts(oldFile), deliveryArtifacts(nextFile)
	authored := deliveryArtifacts(normalized)
	for _, row := range normalized.Artifacts {
		if _, exists := old[row.Ref]; !exists {
			return DeliveryUpdateResult{}, fmt.Errorf("%w: delivery artifact %s is not captured in the previous bundle", ErrInvalid, row.Ref)
		}
	}
	// Each source claims its exact survivor as well as any unambiguous changed
	// selector target. Retaining both prevents silently splitting a path override.
	claims := map[string]map[string]bool{}
	selectors := map[string]map[string]bool{}
	blocked := map[string]bool{}
	addClaim := func(source, target, selector string) {
		if claims[source] == nil {
			claims[source] = map[string]bool{}
		}
		claims[source][target] = true
		if selector != "" {
			if selectors[source] == nil {
				selectors[source] = map[string]bool{}
			}
			selectors[source][selector] = true
		}
	}
	for source := range authored {
		if _, exists := next[source]; exists {
			addClaim(source, source, "")
		}
	}
	oldSelectors, nextSelectors := deliverySelectors(beforeMetadata), deliverySelectors(afterMetadata)
	for _, key := range diffResolutionKeys(oldSelectors, nextSelectors) {
		oldSet, nextSet := oldSelectors[key], nextSelectors[key]
		if slices.Equal(diffKeys(oldSet, nil), diffKeys(nextSet, nil)) || len(oldSet) == 0 || len(nextSet) == 0 {
			continue
		}
		ref, parseErr := registry.ParseReference(key.ref)
		if parseErr != nil {
			return DeliveryUpdateResult{}, fmt.Errorf("invalid captured selector %q: %w", key.ref, parseErr)
		}
		repo := repositoryName(ref)
		sources, targets := []string{}, []string{}
		for _, descriptor := range oldSet {
			source := repo + "@" + descriptor.Digest.String()
			if _, exists := authored[source]; exists {
				sources = append(sources, source)
			}
		}
		for _, descriptor := range nextSet {
			targets = append(targets, repo+"@"+descriptor.Digest.String())
		}
		sources, targets = deliveryUnique(sources), deliveryUnique(targets)
		if len(sources) == 0 {
			continue
		}
		if len(oldSet) != 1 || len(nextSet) != 1 {
			for _, source := range sources {
				blocked[source] = true
			}
			result.Conflicts = append(result.Conflicts, deliveryConflict("selector has multiple captured resolutions", sources, targets, []string{key.kind + " " + key.ref}, authored))
			continue
		}
		if _, exists := next[targets[0]]; !exists {
			return DeliveryUpdateResult{}, fmt.Errorf("invalid captured selector target %s: absent from refreshed inventory", targets[0])
		}
		addClaim(sources[0], targets[0], key.kind+" "+key.ref)
	}
	for _, source := range diffKeys(claims, nil) {
		if len(claims[source]) > 1 {
			blocked[source] = true
			result.Conflicts = append(result.Conflicts, deliveryConflict("source assignment has multiple targets", []string{source}, diffKeys(claims[source], nil), diffKeys(selectors[source], nil), authored))
		}
	}
	incoming := map[string]map[string]bool{}
	for source, targets := range claims {
		if blocked[source] {
			continue
		}
		for target := range targets {
			if incoming[target] == nil {
				incoming[target] = map[string]bool{}
			}
			incoming[target][source] = true
		}
	}
	blockedTargets := map[string]bool{}
	for _, target := range diffKeys(incoming, nil) {
		sources := diffKeys(incoming[target], nil)
		paths, evidence := map[string]bool{}, []string{}
		for _, source := range sources {
			paths[authored[source]] = true
			evidence = append(evidence, diffKeys(selectors[source], nil)...)
		}
		if len(paths) > 1 {
			blockedTargets[target] = true
			for _, source := range sources {
				blocked[source] = true
			}
			result.Conflicts = append(result.Conflicts, deliveryConflict("target receives different explicit paths", sources, []string{target}, evidence, authored))
		}
	}
	// A conflicting target has no safe path action to report, including a
	// default that would otherwise appear after its claims were excluded.
	for source := range blocked {
		for target := range claims[source] {
			blockedTargets[target] = true
		}
	}
	for _, conflict := range result.Conflicts {
		for _, target := range conflict.Targets {
			blockedTargets[target] = true
		}
	}
	moved := map[string]bool{}
	for i := range nextFile.Artifacts {
		row := &nextFile.Artifacts[i]
		sources := diffKeys(incoming[row.Ref], nil)
		if blockedTargets[row.Ref] {
			continue
		}
		assigned := false
		for _, source := range sources {
			if blocked[source] {
				continue
			}
			row.Path = authored[source]
			assigned = true
			change := "moved"
			if source == row.Ref {
				change = "preserved"
			} else {
				moved[source] = true
			}
			result.Changes = append(result.Changes, DeliveryPathChange{Change: change, Before: source, After: row.Ref, Path: row.Path})
		}
		if !assigned {
			result.Changes = append(result.Changes, DeliveryPathChange{Change: "defaulted", After: row.Ref, Path: row.Path})
		}
	}
	for _, row := range oldFile.Artifacts {
		if _, exists := next[row.Ref]; !exists && !moved[row.Ref] && !blocked[row.Ref] {
			result.Changes = append(result.Changes, DeliveryPathChange{Change: "removed", Before: row.Ref, Path: authored[row.Ref]})
		}
	}
	sort.Slice(result.Changes, func(i, j int) bool { return diffJSON(result.Changes[i]) < diffJSON(result.Changes[j]) })
	sort.Slice(result.Conflicts, func(i, j int) bool { return diffJSON(result.Conflicts[i]) < diffJSON(result.Conflicts[j]) })
	result.Conflicts = slices.CompactFunc(result.Conflicts, func(a, b DeliveryPathConflict) bool { return diffJSON(a) == diffJSON(b) })
	if len(result.Conflicts) != 0 {
		return result, fmt.Errorf("%w: %d ambiguous delivery path assignments", ErrConflict, len(result.Conflicts))
	}
	result.Delivery = &nextFile
	return result, nil
}

func deliveryArtifacts(file deliveryfile.File) map[string]string {
	result := map[string]string{}
	for _, row := range file.Artifacts {
		result[row.Ref] = row.Path
	}
	return result
}

func deliverySelectors(metadata []bundle.Metadata) map[diffResolutionKey]diffDescriptorSet {
	result := map[diffResolutionKey]diffDescriptorSet{}
	for _, snapshot := range metadata {
		for key, descriptors := range diffResolutions(snapshot) {
			if result[key] == nil {
				result[key] = diffDescriptorSet{}
			}
			for identity, descriptor := range descriptors {
				result[key][identity] = descriptor
			}
		}
	}
	return result
}

func deliveryUnique(values []string) []string { slices.Sort(values); return slices.Compact(values) }

func deliveryConflict(reason string, sources, targets, selectors []string, authored map[string]string) DeliveryPathConflict {
	result := DeliveryPathConflict{Reason: reason, Sources: []deliveryfile.Artifact{}, Targets: deliveryUnique(targets), Selectors: deliveryUnique(selectors)}
	for _, source := range deliveryUnique(sources) {
		result.Sources = append(result.Sources, deliveryfile.Artifact{Ref: source, Path: authored[source]})
	}
	return result
}
