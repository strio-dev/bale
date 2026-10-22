package config

import (
	"reflect"
	"testing"

	digest "github.com/opencontainers/go-digest"
)

func TestNormalizeDefaultsAndOwnership(t *testing.T) {
	tags := []string{"latest"}
	a, b := digest.FromString("a"), digest.FromString("b")
	input := Recipe{Include: []Selection{{Repository: "example.com/component", Tags: &tags, Referrers: Policy{Require: []RequiredReferrer{{Digest: b}, {Digest: a}, {Digest: b}}}}}}
	before := append([]RequiredReferrer{}, input.Include[0].Referrers.Require...)
	got, err := Normalize(input)
	if err != nil {
		t.Fatal(err)
	}
	if got.Include[0].Referrers.Include != "all" || len(got.Include[0].Referrers.Require) != 2 {
		t.Fatalf("unexpected normalized recipe: %+v", got)
	}
	if !reflect.DeepEqual(before, input.Include[0].Referrers.Require) || input.Include[0].Referrers.Include != "" {
		t.Fatal("normalization mutated input")
	}
	(*got.Include[0].Tags)[0] = "changed"
	got.Include[0].Referrers.Require[0].Digest = digest.FromString("changed")
	if tags[0] != "latest" || !reflect.DeepEqual(before, input.Include[0].Referrers.Require) {
		t.Fatal("normalized recipe aliases caller storage")
	}
}

func TestNormalizeMatchesParsedDefaults(t *testing.T) {
	input := Recipe{Include: []Selection{{Ref: "example.com/component:latest"}}}
	normalized, err := Normalize(input)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse([]byte("include:\n  - example.com/component:latest\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized, parsed) {
		t.Fatalf("Go and YAML defaults differ: %#v %#v", normalized, parsed)
	}
}

func TestNormalizeRejectsInvalidInput(t *testing.T) {
	for _, input := range []Recipe{
		{},
		{Include: []Selection{{Ref: "example.com/component:latest", Referrers: Policy{Include: "unknown"}}}},
		{Include: []Selection{{Ref: "example.com/component:latest", Referrers: Policy{Require: []RequiredReferrer{{Digest: "sha256:bad"}}}}}},
	} {
		if _, err := Normalize(input); err == nil {
			t.Fatalf("accepted invalid recipe: %#v", input)
		}
	}
}
