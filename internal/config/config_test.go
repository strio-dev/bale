package config

import (
	"encoding/json"
	"strings"
	"testing"
)

const sha = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParsePoliciesAndTags(t *testing.T) {
	r, err := Parse([]byte("defaults:\n  referrers:\n    include: none\n    require:\n      - digest: " + sha + "\ninclude:\n  - example.com/one:stable\n  - repository: example.com/two\n    tags: []\n    referrers:\n      include: all\n      require: []\n  - repository: localhost:5000/three\n    tags: [all, stable]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Include[0].Referrers.Include != "none" || len(r.Include[0].Referrers.Require) != 1 {
		t.Fatalf("lost inherited policy: %+v", r)
	}
	if r.Include[1].Tags == nil || len(*r.Include[1].Tags) != 0 || r.Include[1].Referrers.Include != "all" || r.Include[1].Referrers.Require == nil || len(r.Include[1].Referrers.Require) != 0 {
		t.Fatalf("lost replacements: %+v", r.Include[1])
	}
	if (*r.Include[2].Tags)[0] != "all" {
		t.Fatal("all must remain a literal tag")
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "defaults") || !strings.Contains(string(data), `"require":[]`) {
		t.Fatalf("not normalized JSON: %s", data)
	}
	var decoded Recipe
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := Validate(decoded); err != nil {
		t.Fatal(err)
	}
}

func TestRequireSortedUnique(t *testing.T) {
	d2 := "sha256:1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	r, err := Parse([]byte("include:\n  - ref: example.com/one:tag\n    referrers:\n      include: none\n      require:\n        - digest: " + d2 + "\n        - digest: " + sha + "\n        - digest: " + sha + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Include[0].Referrers.Require) != 2 || string(r.Include[0].Referrers.Require[0].Digest) != sha {
		t.Fatalf("not canonical: %+v", r)
	}
}

func TestRejectMalformedRecipes(t *testing.T) {
	cases := map[string]string{
		"empty": "", "empty inputs": "include: []", "top list": "- example.com/a:tag",
		"unknown": "include: [example.com/a:tag]\nname: x", "duplicate": "include: []\ninclude: [example.com/a:tag]",
		"null": "defaults: null\ninclude: [example.com/a:tag]", "multi document": "include: [example.com/a:tag]\n---\ninclude: [example.com/b:tag]",
		"number": "include: [42]", "boolean": "include: [true]", "anchor": "include: [&x example.com/a:tag]",
		"alias": "include: [&x example.com/a:tag, *x]", "no registry": "include: [team/image:tag]",
		"implicit tag": "include: [example.com/a]", "scheme": "include: [https://example.com/a:tag]",
		"missing tags": "include:\n  - repository: example.com/a", "scalar tags": "include:\n  - repository: example.com/a\n    tags: all",
		"null tags": "include:\n  - repository: example.com/a\n    tags: null", "numeric tag": "include:\n  - repository: example.com/a\n    tags: [12]",
		"duplicate tag": "include:\n  - repository: example.com/a\n    tags: [x, x]",
		"conflict":      "include:\n  - ref: example.com/a:tag\n    repository: example.com/a\n    tags: []",
		"ref tags":      "include:\n  - ref: example.com/a:tag\n    tags: []", "tagged repo": "include:\n  - repository: example.com/a:tag\n    tags: []",
		"tag digest":      "include: [example.com/a:tag@" + sha + "]",
		"aliases":         "include:\n  - ref: example.com/a:tag\n    as: x\n  - ref: example.com/b:tag\n    as: x",
		"old attachments": "include:\n  - ref: example.com/a:tag\n    attachments: {}", "platform": "include:\n  - ref: example.com/a:tag\n    platforms: []",
		"bad include":         "include:\n  - ref: example.com/a:tag\n    referrers:\n      include: some",
		"bad require":         "include:\n  - ref: example.com/a:tag\n    referrers:\n      require: [" + sha + "]",
		"bad digest":          "include:\n  - ref: example.com/a:tag\n    referrers:\n      require: [{digest: sha256:abc}]",
		"unknown requirement": "include:\n  - ref: example.com/a:tag\n    referrers:\n      require: [{digest: " + sha + ", artifactType: x}]",
		"empty mapping":       "include: [{}]", "whitespace": "include: [' example.com/a:tag']",
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(source)); err == nil {
				t.Fatalf("accepted %s", source)
			}
		})
	}
}

func TestDigestAndLocalhost(t *testing.T) {
	for _, ref := range []string{"example.com/a@" + sha, "localhost/a:tag", "localhost:5000/a:tag", "[::1]:5000/a:tag"} {
		if _, err := Parse([]byte("include: ['" + ref + "']")); err != nil {
			t.Errorf("%s: %v", ref, err)
		}
	}
}
