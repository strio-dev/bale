package dependency

import (
	"encoding/json"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestDeclarationRoundTripCanonical(t *testing.T) {
	input := Declaration{Dependencies: []Reference{
		{Ref: "example.com/z:tag"}, {Ref: "example.com/a:tag", Follow: "auto"},
		{Ref: "example.com/z:tag", Follow: "auto"}, {Ref: "example.com/a:tag", Follow: "none"},
		{Ref: "example.com/a:tag", Select: &Selector{Context: "production"}},
	}}
	raw, err := Encode(input)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != Version || len(decoded.Dependencies) != 4 {
		t.Fatalf("not canonical: %s", raw)
	}
	again, err := Encode(decoded)
	if err != nil || string(raw) != string(again) {
		t.Fatalf("unstable encoding: %s %v", again, err)
	}
	if input.Dependencies[1].Follow != "auto" {
		t.Fatal("mutated caller")
	}
	empty, err := Encode(Declaration{})
	if err != nil || string(empty) != `{"schemaVersion":1,"dependencies":[]}` {
		t.Fatalf("empty author helper: %s %v", empty, err)
	}
}

func TestDecodeRejectsMalformedDeclarations(t *testing.T) {
	cases := []string{
		`null`, `[]`, `{}`, `{"schemaVersion":1}`, `{"schemaVersion":0,"dependencies":[]}`,
		`{"schemaVersion":2,"dependencies":[]}`, `{"schemaVersion":1,"dependencies":null}`,
		`{"schemaVersion":1,"dependencies":[],"dependencies":[]}`,
		`{"schemaVersion":1,"dependencies":[]} {}`,
		`{"schemaVersion":1,"dependencies":[],"unknown":true}`,
		`{"schemaVersion":1,"dependencies":[],"Context":"prod"}`,
		`{"schemaVersion":1,"dependencies":[],"context":""}`,
		`{"schemaVersion":1,"dependencies":[],"context":" prod"}`,
		`{"schemaVersion":1,"dependencies":[null]}`,
		`{"schemaVersion":1,"dependencies":[{}]}`,
		`{"schemaVersion":1,"dependencies":[{"ref":"example.com/a:tag","id":"x"}]}`,
		`{"schemaVersion":1,"dependencies":[{"ref":"example.com/a:tag","follow":""}]}`,
		`{"schemaVersion":1,"dependencies":[{"ref":"example.com/a:tag","follow":"sometimes"}]}`,
		`{"schemaVersion":1,"dependencies":[{"ref":"example.com/a:tag","select":{}}]}`,
		`{"schemaVersion":1,"dependencies":[{"ref":"example.com/a:tag","select":{"context":""}}]}`,
		`{"schemaVersion":1,"dependencies":[{"ref":"example.com/a:tag","select":{"context":null}}]}`,
		`{"schemaVersion":1,"dependencies":[{"ref":"example.com/a:tag","select":{"context":"prod","Context":"dev"}}]}`,
		`{"schemaVersion":1,"dependencies":[{"ref":"example.com/a:tag","select":{"context":"prod","digest":"` + testDigest + `"}}]}`,
		`{"schemaVersion":1,"dependencies":[{"ref":"example.com/a:tag","follow":"none","select":{"context":"prod"}}]}`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if _, err := Decode([]byte(raw)); err == nil {
				t.Fatalf("accepted %s", raw)
			}
		})
	}
}

func TestValidateReference(t *testing.T) {
	for _, value := range []string{"example.com/a:tag", "localhost/a:tag", "localhost:5000/a@" + testDigest, "[::1]:5000/a:tag"} {
		if err := ValidateReference(value); err != nil {
			t.Errorf("%s: %v", value, err)
		}
	}
	for _, value := range []string{"", "team/a:tag", "example.com/a", "https://example.com/a:tag", "example.com/a:tag@" + testDigest, " example.com/a:tag", "example.com/a@sha256:bad"} {
		if err := ValidateReference(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestPolicyCopiesAndValidatesSelector(t *testing.T) {
	original := Policy{Select: &Selector{Digest: digest.Digest(testDigest)}}
	normalized, err := NormalizePolicy(original)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Follow != "auto" || normalized.Select == original.Select {
		t.Fatal("missing default or deep copy")
	}
	normalized.Select.Context = "mutated"
	if original.Select.Context != "" {
		t.Fatal("mutated original selector")
	}
	for _, p := range []Policy{{Follow: "x"}, {Select: &Selector{}}, {Select: &Selector{Digest: "bad"}}, {Select: &Selector{Context: " "}}, {Follow: "none", Select: &Selector{Context: "prod"}}} {
		if _, err := NormalizePolicy(p); err == nil {
			raw, _ := json.Marshal(p)
			t.Errorf("accepted %s", raw)
		}
	}
	if _, err := Decode([]byte(`{"schemaVersion":1,"dependencies":[{"ref":"example.com/a@` + testDigest + `","select":{"digest":"` + testDigest + `"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := Encode(Declaration{Context: strings.Repeat(" ", 2)}); err == nil {
		t.Fatal("accepted whitespace context")
	}
}

func TestDeclarationSizeLimit(t *testing.T) {
	if _, err := Encode(Declaration{Context: strings.Repeat("x", int(MaxBytes))}); err == nil {
		t.Fatal("encoded oversized declaration")
	}
	if _, err := Decode([]byte(strings.Repeat(" ", int(MaxBytes)+1))); err == nil {
		t.Fatal("decoded oversized declaration")
	}
}
