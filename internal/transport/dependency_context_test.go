package transport

import (
	"testing"

	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
)

func TestDependencyCaptureSharesBytesAcrossSelectedContexts(t *testing.T) {
	r, repo, service := testRegistry(t)
	root := object(t, r.store, "shared component")
	left := object(t, r.store, "development dependency")
	right := object(t, r.store, "production dependency")
	r.tags["root"], r.tags["left"], r.tags["right"] = root, left, right
	dependencyDeclaration(t, r, root, "development", dependency.Reference{Ref: repo + ":left"})
	dependencyDeclaration(t, r, root, "production", dependency.Reference{Ref: repo + ":right"})
	selections := []config.Selection{}
	for _, context := range []string{"development", "production"} {
		input := dependencySelection(repo + ":root")
		input.Dependencies.Select = &dependency.Selector{Context: context}
		selections = append(selections, input)
	}
	_, snapshot, err := service.capture(ctx, config.Recipe{Include: selections})
	if err != nil {
		t.Fatal(err)
	}
	if !capturedOrigin(snapshot.Metadata, left) || !capturedOrigin(snapshot.Metadata, right) {
		t.Fatal("content deduplication suppressed a selected context")
	}
	if len(snapshot.Metadata.Referrers) != 2 || r.headTags["root"] != 1 {
		t.Fatal("declaration choices or shared source pin were lost")
	}
}
