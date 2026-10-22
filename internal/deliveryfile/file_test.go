package deliveryfile

import (
	"reflect"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
)

func TestRoundTripAndDefensiveOrdering(t *testing.T) {
	d := digest.FromString("bundle")
	a := Artifact{Ref: "registry.example/z@" + digest.FromString("z").String(), Path: "apps/z"}
	b := Artifact{Ref: "registry.example/a@" + digest.FromString("a").String(), Path: "apps/a"}
	file := File{Bundle: d, Artifacts: []Artifact{a, b}}
	encoded, err := Encode(file)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(file.Artifacts, []Artifact{a, b}) {
		t.Fatal("encoding mutated caller order")
	}
	decoded, err := Decode(encoded)
	if err != nil || decoded.Bundle != d || !reflect.DeepEqual(decoded.Artifacts, []Artifact{b, a}) {
		t.Fatalf("round trip: %+v %v", decoded, err)
	}
	again, err := Encode(decoded)
	if err != nil || string(again) != string(encoded) {
		t.Fatalf("unstable encoding: %s %v", again, err)
	}
	decoded.Artifacts[0].Path = "changed"
	if file.Artifacts[1].Path != "apps/a" {
		t.Fatal("normalization shared input storage")
	}
}

func TestDecodeRejectsMalformedFiles(t *testing.T) {
	d := digest.FromString("bundle").String()
	r := "registry.example/apps/web@" + digest.FromString("web").String()
	valid := "bundle: " + d + "\nartifacts:\n  - ref: " + r + "\n    path: apps/web\n"
	cases := map[string]string{
		"missing bundle":       "artifacts: []\n",
		"missing artifacts":    "bundle: " + d + "\n",
		"unknown":              valid + "other: value\n",
		"unknown nested":       valid + "    other: value\n",
		"duplicate key":        valid + "bundle: " + d + "\n",
		"duplicate nested key": valid + "    path: other\n",
		"duplicate identity":   valid + "  - ref: " + r + "\n    path: other\n",
		"null":                 strings.Replace(valid, "path: apps/web", "path: null", 1),
		"numeric":              strings.Replace(valid, "path: apps/web", "path: 42", 1),
		"alias":                strings.Replace(valid, "path: apps/web", "path: &p apps/web", 1),
		"multiple documents":   valid + "---\n" + valid,
		"tag ref":              strings.Replace(valid, r, "registry.example/apps/web:v1", 1),
		"tag and digest":       strings.Replace(valid, "/web@", "/web:v1@", 1),
		"implicit registry":    strings.Replace(valid, "registry.example/", "registry/", 1),
		"invalid digest":       strings.Replace(valid, d, "sha256:bad", 1),
		"empty":                "",
		"scalar":               "hello\n",
		"missing ref":          strings.Replace(valid, "ref: "+r, "", 1),
		"missing path":         strings.Replace(valid, "    path: apps/web\n", "", 1),
	}
	for _, path := range []string{"/absolute", "../parent", "apps/../escape", "apps//web", "other.example/repo:tag", "Mixed/Case", "", " apps/web", "apps/web "} {
		cases["path "+path] = strings.Replace(valid, "path: apps/web", "path: \""+path+"\"", 1)
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(input)); err == nil {
				t.Fatal("accepted invalid delivery file")
			}
		})
	}
}

func TestEmptyOverridesRetainBundleBinding(t *testing.T) {
	file := File{Bundle: digest.FromString("bundle")}
	encoded, err := Encode(file)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil || decoded.Artifacts == nil || len(decoded.Artifacts) != 0 {
		t.Fatalf("empty overrides: %+v %v", decoded, err)
	}
}
