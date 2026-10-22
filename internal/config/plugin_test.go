package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPluginInputJSONAndIndependentNormalization(t *testing.T) {
	recipe, err := Parse([]byte(`include:
  - ref: example.com/charts/app:1
    plugin:
      ref: example.com/plugins/discover:1
      input:
        big: 9007199254740993
        max: 18446744073709551615
        larger: 1844674407370955161612345678901234567890
        schemaVersion: 1
        values: {enabled: true, disabled: false, absent: null, nested: [null, 2, ""]}
      blobs:
        chart: {mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip}
`))
	if err != nil {
		t.Fatal(err)
	}
	input := string(recipe.Include[0].Plugin.Input)
	for _, want := range []string{`"big":9007199254740993`, `"max":18446744073709551615`, `"larger":1844674407370955161612345678901234567890`, `"absent":null`, `"nested":[null,2,""]`, `"enabled":true`} {
		if !strings.Contains(input, want) {
			t.Fatalf("lost JSON semantics: %s", input)
		}
	}
	normalized, err := Normalize(recipe)
	if err != nil {
		t.Fatal(err)
	}
	recipe.Include[0].Plugin.Input[0] = '['
	recipe.Include[0].Plugin.Blobs["chart"] = PluginBlob{MediaType: "changed"}
	if normalized.Include[0].Plugin.Input[0] != '{' || normalized.Include[0].Plugin.Blobs["chart"].MediaType == "changed" {
		t.Fatal("plugin normalization aliases caller data")
	}
	defaulted, err := Parse([]byte("include:\n  - ref: example.com/chart:1\n    plugin: {ref: example.com/plugin:1}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if string(defaulted.Include[0].Plugin.Input) != "{}" {
		t.Fatal("missing input default")
	}
}

func TestRejectInvalidPluginRecipes(t *testing.T) {
	base := "include:\n  - ref: example.com/chart:1\n    plugin:\n"
	cases := map[string]string{
		"missing ref":       "      input: {}\n",
		"implicit tag":      "      ref: example.com/plugin\n",
		"implicit registry": "      ref: team/plugin:1\n",
		"unknown":           "      ref: example.com/plugin:1\n      inputs: {}\n",
		"duplicate":         "      ref: example.com/plugin:1\n      input: {x: 1, x: 2}\n",
		"alias":             "      ref: example.com/plugin:1\n      input: {x: &x 1, y: *x}\n",
		"key anchor":        "      ref: example.com/plugin:1\n      input: {&key x: 1}\n",
		"custom map tag":    "      ref: example.com/plugin:1\n      input: !foo {x: 1}\n",
		"custom seq tag":    "      ref: example.com/plugin:1\n      input: {x: !foo [1]}\n",
		"nonstring key":     "      ref: example.com/plugin:1\n      input: {1: x}\n",
		"nonobject":         "      ref: example.com/plugin:1\n      input: []\n",
		"null input":        "      ref: example.com/plugin:1\n      input: null\n",
		"nonfinite":         "      ref: example.com/plugin:1\n      input: {x: .inf}\n",
		"timestamp":         "      ref: example.com/plugin:1\n      input: {x: 2026-10-19}\n",
		"blank blob":        "      ref: example.com/plugin:1\n      blobs: {'': {mediaType: example/type}}\n",
		"missing mediaType": "      ref: example.com/plugin:1\n      blobs: {chart: {}}\n",
		"unknown blob":      "      ref: example.com/plugin:1\n      blobs: {chart: {mediaType: example/type, path: foo}}\n",
		"collision":         "      ref: example.com/plugin:1\n      input: {chart: null}\n      blobs: {chart: {mediaType: example/type}}\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(base + body)); err == nil {
				t.Fatal("accepted invalid plugin")
			}
		})
	}
	for _, policy := range []string{"{follow: none}", "{select: {context: prod}}"} {
		if _, err := Parse([]byte("include:\n  - ref: example.com/chart:1\n    dependencies: " + policy + "\n    plugin: {ref: example.com/plugin:1}\n")); err == nil {
			t.Fatalf("accepted competing policy %s", policy)
		}
	}
}

func TestPluginProgrammaticInputValidation(t *testing.T) {
	for _, input := range []json.RawMessage{json.RawMessage(`{"x":1,"x":2}`), json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`{"x":"` + strings.Repeat("x", MaxPluginInputBytes) + `"}`)} {
		_, err := Normalize(Recipe{Include: []Selection{{Ref: "example.com/chart:1", Plugin: &Plugin{Ref: "example.com/plugin:1", Input: input}}}})
		if err == nil {
			t.Fatal("accepted invalid plugin input")
		}
	}
}
