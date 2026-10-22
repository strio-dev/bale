package config

import (
	"testing"

	"github.com/strio-dev/bale/internal/dependency"
)

func TestDependencyPoliciesDefaultsAndReplacements(t *testing.T) {
	r, err := Parse([]byte(`defaults:
  dependencies:
    select:
      digest: ` + sha + `
include:
  - example.com/a:tag
  - ref: example.com/b:tag
    dependencies:
      select:
        context: production
  - ref: example.com/c:tag
    dependencies:
      follow: none
`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Include[0].Dependencies.Follow != "auto" || string(r.Include[0].Dependencies.Select.Digest) != sha {
		t.Fatalf("lost defaults: %+v", r)
	}
	if r.Include[1].Dependencies.Select.Context != "production" || r.Include[1].Dependencies.Select.Digest != "" {
		t.Fatal("selector merged rather than replaced")
	}
	if r.Include[2].Dependencies.Follow != "none" || r.Include[2].Dependencies.Select != nil {
		t.Fatal("disable retained inherited selector")
	}
	r.Include[0].Dependencies.Select.Context = "mutated"
	if r.Include[1].Dependencies.Select.Context != "production" {
		t.Fatal("shared selector storage")
	}
	normalized, err := Normalize(Recipe{Include: []Selection{{Ref: "example.com/a:tag", Dependencies: dependency.Policy{Select: &dependency.Selector{Context: "prod"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	again, err := Normalize(normalized)
	if err != nil {
		t.Fatal(err)
	}
	again.Include[0].Dependencies.Select.Context = "other"
	if normalized.Include[0].Dependencies.Select.Context != "prod" {
		t.Fatal("Normalize aliases selector")
	}
}

func TestDependencyPolicyRejectsMalformedYAML(t *testing.T) {
	cases := []string{
		"follow: true", "follow: sometimes", "follow: null", "follow: ''", "unknown: auto",
		"select: {}", "select: null", "select: {context: ''}", "select: {digest: bad}",
		"select: {context: prod, digest: " + sha + "}", "select: {context: prod, context: dev}",
		"follow: none\n      select: {context: prod}",
	}
	for _, source := range cases {
		t.Run(source, func(t *testing.T) {
			if _, err := Parse([]byte("include:\n  - ref: example.com/a:tag\n    dependencies:\n      " + source + "\n")); err == nil {
				t.Fatalf("accepted %s", source)
			}
		})
	}
}
