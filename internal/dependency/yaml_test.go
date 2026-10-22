package dependency

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseYAMLUsesRegistrySemantics(t *testing.T) {
	input := `context: production
dependencies:
  - ref: registry.example/images/web:stable
    follow: auto
  - ref: registry.example/images/web:stable
  - ref: registry.example/components/runtime:v1
    select:
      context: production
  - ref: registry.example/images/database:17
    follow: none
`
	parsed, err := ParseYAML([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.SchemaVersion != Version || parsed.Context != "production" || len(parsed.Dependencies) != 3 {
		t.Fatalf("unexpected parsed declaration: %+v", parsed)
	}
	raw, err := Encode(parsed)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(raw)
	if err != nil || !reflect.DeepEqual(parsed, decoded) {
		t.Fatalf("authoring and registry semantics differ: %+v %v", decoded, err)
	}
	for _, reference := range parsed.Dependencies {
		if reference.Follow == "auto" {
			t.Fatal("default policy was not canonicalized")
		}
	}
	empty, err := ParseYAML([]byte("dependencies: []\n"))
	if err != nil || empty.SchemaVersion != Version || empty.Dependencies == nil || len(empty.Dependencies) != 0 {
		t.Fatalf("explicit empty dependencies: %+v %v", empty, err)
	}
}

func TestParseYAMLRejectsAmbiguousAuthoring(t *testing.T) {
	valid := "dependencies:\n  - ref: registry.example/images/web:stable\n"
	cases := map[string]string{
		"missing dependencies":     "context: production\n",
		"unknown root":             valid + "other: value\n",
		"unknown entry":            valid + "    other: value\n",
		"duplicate root":           valid + "dependencies: []\n",
		"duplicate entry":          valid + "    ref: registry.example/images/web:other\n",
		"schema version":           "schemaVersion: '1'\n" + valid,
		"numeric version":          "schemaVersion: 1\n" + valid,
		"missing ref":              "dependencies:\n  - follow: none\n",
		"scalar entry":             "dependencies:\n  - registry.example/images/web:stable\n",
		"null":                     "dependencies: null\n",
		"alias":                    "dependencies: &entries []\n",
		"numeric":                  valid + "    follow: 42\n",
		"empty context":            "context: ''\n" + valid,
		"context whitespace":       "context: ' production'\n" + valid,
		"tag missing":              strings.Replace(valid, ":stable", "", 1),
		"registry implicit":        strings.Replace(valid, "registry.example", "registry", 1),
		"tag and digest":           strings.Replace(valid, ":stable", ":stable@sha256:"+strings.Repeat("a", 64), 1),
		"unknown follow":           valid + "    follow: always\n",
		"disabled selection":       valid + "    follow: none\n    select: {context: production}\n",
		"selection empty":          valid + "    select: {}\n",
		"selection unknown":        valid + "    select: {other: value}\n",
		"selection both":           valid + "    select: {context: production, digest: 'sha256:" + strings.Repeat("a", 64) + "'}\n",
		"selection invalid digest": valid + "    select: {digest: 'sha256:bad'}\n",
		"selection null":           valid + "    select: null\n",
		"multiple documents":       valid + "---\n" + valid,
		"empty":                    "",
		"root scalar":              "not-a-mapping\n",
		"too large":                strings.Repeat(" ", int(MaxBytes)+1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseYAML([]byte(input)); err == nil {
				t.Fatal("accepted malformed declaration YAML")
			}
		})
	}
}
