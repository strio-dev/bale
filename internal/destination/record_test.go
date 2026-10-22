package destination

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
)

func fixture() Record {
	subject := digest.FromString("subject")
	child := digest.FromString("child")
	return Record{Bundle: "target.example/bundles@" + digest.FromString("bundle").String(), Source: "source.example/charts/web@" + subject.String(), Destination: "target.example/charts/web@" + subject.String(), Resolutions: []Resolution{{Capture: digest.FromString("capture"), Outcome: "selected", Declaration: &Declaration{Digest: digest.FromString("declaration"), Context: "production"}, Dependencies: []Dependency{{Ref: "source.example/images/web:stable", Source: "source.example/images/web@" + child.String(), Destination: "target.example/images/web@" + child.String()}}}}}
}
func TestCanonicalRecord(t *testing.T) {
	r := fixture()
	r.Resolutions[0].Dependencies = append(r.Resolutions[0].Dependencies, r.Resolutions[0].Dependencies[0])
	r.Resolutions = append(r.Resolutions, r.Resolutions[0])
	raw, err := Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != 1 || len(decoded.Resolutions) != 1 || len(decoded.Resolutions[0].Dependencies) != 1 || decoded.Resolutions[0].Policy.Follow != "auto" {
		t.Fatalf("unexpected normalization: %+v", decoded)
	}
	again, err := Encode(decoded)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("unstable encoding: %v", err)
	}
	empty := fixture()
	empty.Resolutions = nil
	raw, err = Encode(empty)
	if err != nil || !bytes.Contains(raw, []byte(`"resolutions":[]`)) {
		t.Fatalf("empty array: %s %v", raw, err)
	}
	// Identical bytes from a different source repository remain distinct dependencies.
	r = fixture()
	other := r.Resolutions[0].Dependencies[0]
	other.Ref = "other.example/web:stable"
	other.Source = "other.example/web@" + digest.FromString("child").String()
	r.Resolutions[0].Dependencies = append(r.Resolutions[0].Dependencies, other)
	raw, err = Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = Decode(raw)
	if err != nil || len(decoded.Resolutions[0].Dependencies) != 2 {
		t.Fatalf("repository identity lost: %v", err)
	}
}
func TestRejectRecordSemantics(t *testing.T) {
	cases := map[string]func(*Record){
		"version":                      func(r *Record) { r.SchemaVersion = 2 },
		"unpinned bundle":              func(r *Record) { r.Bundle = "target.example/bundle:tag" },
		"mismatched subject":           func(r *Record) { r.Destination = "target.example/web@" + digest.FromString("different").String() },
		"unknown capture":              func(r *Record) { r.Resolutions[0].Capture = "bad" },
		"unknown outcome":              func(r *Record) { r.Resolutions[0].Outcome = "pending" },
		"selected missing declaration": func(r *Record) { r.Resolutions[0].Declaration = nil },
		"selected disabled":            func(r *Record) { r.Resolutions[0].Policy.Follow = "none" },
		"mismatched context":           func(r *Record) { r.Resolutions[0].Policy.Select = &dependency.Selector{Context: "other"} },
		"mismatched declaration": func(r *Record) {
			r.Resolutions[0].Policy.Select = &dependency.Selector{Digest: digest.FromString("other")}
		},
		"padded context":           func(r *Record) { r.Resolutions[0].Declaration.Context = " production" },
		"absent with dependencies": func(r *Record) { r.Resolutions[0].Outcome = "absent"; r.Resolutions[0].Declaration = nil },
		"disabled with enabled policy": func(r *Record) {
			r.Resolutions[0].Outcome = "disabled"
			r.Resolutions[0].Declaration = nil
			r.Resolutions[0].Dependencies = nil
		},
		"wrong dependency repository": func(r *Record) { r.Resolutions[0].Dependencies[0].Ref = "other.example/images/web:stable" },
		"wrong pinned dependency": func(r *Record) {
			r.Resolutions[0].Dependencies[0].Ref = "source.example/images/web@" + digest.FromString("other").String()
		},
		"wrong dependency digest": func(r *Record) {
			r.Resolutions[0].Dependencies[0].Destination = "target.example/web@" + digest.FromString("other").String()
		},
		"conflicting mapping": func(r *Record) {
			d := r.Resolutions[0].Dependencies[0]
			d.Destination = "target.example/elsewhere@" + digest.FromString("child").String()
			r.Resolutions[0].Dependencies = append(r.Resolutions[0].Dependencies, d)
		},
		"conflicting capture": func(r *Record) {
			other := r.Resolutions[0]
			other.Declaration = &Declaration{Digest: digest.FromString("other")}
			r.Resolutions = append(r.Resolutions, other)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := fixture()
			change(&r)
			if _, err := Encode(r); err == nil {
				t.Fatal("accepted invalid record")
			}
		})
	}
	for _, outcome := range []string{"absent", "disabled"} {
		r := fixture()
		r.Resolutions[0].Outcome = outcome
		r.Resolutions[0].Declaration = nil
		r.Resolutions[0].Dependencies = nil
		if outcome == "disabled" {
			r.Resolutions[0].Policy.Follow = "none"
		}
		if _, err := Encode(r); err != nil {
			t.Fatal(err)
		}
	}
}
func TestRejectAmbiguousJSON(t *testing.T) {
	raw, _ := Encode(fixture())
	valid := string(raw)
	cases := []string{
		valid + " {}", strings.Replace(valid, `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1), strings.Replace(valid, `"schemaVersion":1`, `"SchemaVersion":1`, 1), strings.Replace(valid, `"schemaVersion":1`, `"schemaVersion":0`, 1),
		strings.Replace(valid, `"resolutions":[`, `"unknown":true,"resolutions":[`, 1), strings.Replace(valid, `"resolutions":[`, `"resolutions":null,"extra":[`, 1),
		strings.Replace(valid, `"follow":"auto"`, `"follow":""`, 1), strings.Replace(valid, `"follow":"auto"`, `"Follow":"auto"`, 1), strings.Replace(valid, `"context":"production"`, `"context":""`, 1),
		strings.Replace(valid, `"dependencies":[`, `"dependencies":{},"extra":[`, 1), `[]`, `null`, strings.Repeat(" ", int(MaxBytes)+1),
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	delete(fields, "resolutions")
	missing, _ := json.Marshal(fields)
	cases = append(cases, string(missing))
	for i, input := range cases {
		if _, err := Decode([]byte(input)); err == nil {
			t.Fatalf("accepted invalid input %d: %s", i, input)
		}
	}
}
func subject() v1.Descriptor {
	return v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString("subject"), Size: 123}
}
func TestEnvelopeRoundTrip(t *testing.T) {
	ctx := context.Background()
	r := fixture()
	d, store, err := Prepare(ctx, subject(), r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Load(ctx, store, d, subject())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := Encode(got)
	want, _ := Encode(r)
	if !bytes.Equal(raw, want) {
		t.Fatal("record changed")
	}
	again, _, err := Prepare(ctx, subject(), r)
	if err != nil || again.Digest != d.Digest {
		t.Fatalf("non deterministic: %v", err)
	}
	wrong := subject()
	wrong.Size++
	if _, err := Load(ctx, store, d, wrong); err == nil {
		t.Fatal("accepted wrong subject")
	}
	wrong = subject()
	wrong.Digest = digest.FromString("other")
	if _, _, err := Prepare(ctx, wrong, r); err == nil {
		t.Fatal("accepted record for wrong subject")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Load(canceled, store, d, subject()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if _, _, err := Prepare(canceled, subject(), r); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
func TestRejectEnvelope(t *testing.T) {
	ctx := context.Background()
	d, store, err := Prepare(ctx, subject(), fixture())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.Fetch(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	var original v1.Manifest
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*v1.Manifest){
		"wrong type":         func(m *v1.Manifest) { m.ArtifactType = "other" },
		"missing subject":    func(m *v1.Manifest) { m.Subject = nil },
		"wrong subject type": func(m *v1.Manifest) { s := *m.Subject; s.MediaType = v1.MediaTypeImageIndex; m.Subject = &s },
		"extra opaque layer": func(m *v1.Manifest) {
			m.Layers = append(m.Layers, v1.Descriptor{MediaType: "opaque", Digest: digest.FromString("x"), Size: 1})
		},
		"missing layer":       func(m *v1.Manifest) { m.Layers = nil },
		"wrong config":        func(m *v1.Manifest) { m.Config.MediaType = "other" },
		"wrong config digest": func(m *v1.Manifest) { m.Config.Digest = digest.FromString("[]"); m.Config.Data = []byte("[]") },
		"oversized payload":   func(m *v1.Manifest) { m.Layers[0].Size = MaxBytes + 1 },
		"corrupt payload":     func(m *v1.Manifest) { m.Layers[0].Data = []byte("xx") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			m := original
			m.Layers = append([]v1.Descriptor(nil), original.Layers...)
			change(&m)
			raw, _ := json.Marshal(m)
			desc := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, raw)
			desc.Data = raw
			if _, err := Load(ctx, store, desc, subject()); err == nil {
				t.Fatal("accepted invalid envelope")
			}
		})
	}
	oversized := d
	oversized.Size = MaxBytes + 1
	if _, err := Load(ctx, store, oversized, subject()); err == nil {
		t.Fatal("accepted oversized manifest")
	}
	corrupt := d
	corrupt.Data = []byte("corrupt")
	if _, err := Load(ctx, store, corrupt, subject()); err == nil {
		t.Fatal("accepted corrupt manifest")
	}
}

func TestCaptureScopeAndPolicies(t *testing.T) {
	r := fixture()
	original := r.Resolutions[0]
	second := original
	second.Capture = digest.FromString("nested capture")
	second.Dependencies = append([]Dependency(nil), original.Dependencies...)
	second.Dependencies[0].Source = "source.example/images/web@" + digest.FromString("later child").String()
	second.Dependencies[0].Destination = "target.example/images/web@" + digest.FromString("later child").String()
	r.Resolutions = append(r.Resolutions, second)
	raw, err := Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil || len(got.Resolutions) != 2 {
		t.Fatalf("nested scope lost: %v", err)
	}
	// The same mutable reference cannot acquire different digests inside one capture,
	// even when declaration selection differs.
	r.Resolutions[1].Capture = original.Capture
	r.Resolutions[1].Policy.Select = &dependency.Selector{Context: "production"}
	if _, err := Encode(r); err == nil {
		t.Fatal("accepted inconsistent reference within one capture")
	}
	// Distinct policies on identical direct dependency references remain distinct.
	r = fixture()
	other := r.Resolutions[0].Dependencies[0]
	other.Policy.Follow = "none"
	r.Resolutions[0].Dependencies = append(r.Resolutions[0].Dependencies, other)
	raw, err = Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	got, err = Decode(raw)
	if err != nil || len(got.Resolutions[0].Dependencies) != 2 {
		t.Fatalf("distinct dependency policy lost: %v", err)
	}
	// Normalization must not mutate caller slices or policy pointers.
	if r.SchemaVersion != 0 || r.Resolutions[0].Policy.Follow != "" || r.Resolutions[0].Dependencies[0].Policy.Follow != "" {
		t.Fatal("encoding mutated input")
	}
	r = fixture()
	r.Resolutions[0].Declaration.Context = strings.Repeat("x", int(MaxBytes))
	if _, err := Encode(r); err == nil {
		t.Fatal("accepted oversized encoded payload")
	}
}

type oversizedStore struct{ reads int }
type countedReader struct {
	remaining int64
	owner     *oversizedStore
}

func (r *countedReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), r.remaining)
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.remaining -= n
	r.owner.reads += int(n)
	return int(n), nil
}
func (*countedReader) Close() error { return nil }
func (s *oversizedStore) Fetch(context.Context, v1.Descriptor) (io.ReadCloser, error) {
	return &countedReader{remaining: MaxBytes * 2, owner: s}, nil
}
func (*oversizedStore) Exists(context.Context, v1.Descriptor) (bool, error) { return true, nil }
func TestReadIsBoundedByAdvertisedSize(t *testing.T) {
	store := &oversizedStore{}
	desc := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, []byte("tiny"))
	if _, err := Load(context.Background(), store, desc, subject()); err == nil {
		t.Fatal("accepted dishonest stream")
	}
	if store.reads != int(desc.Size)+1 {
		t.Fatalf("read %d bytes for size %d", store.reads, desc.Size)
	}
}

func TestCanonicalDestinationAcrossRecord(t *testing.T) {
	for _, scenario := range []string{"aliases", "policies", "captures", "subject loop"} {
		t.Run(scenario, func(t *testing.T) {
			r := fixture()
			d := r.Resolutions[0].Dependencies[0]
			d.Destination = "target.example/alternate@" + digest.FromString("child").String()
			switch scenario {
			case "aliases":
				d.Ref = "source.example/images/web:another"
				r.Resolutions[0].Dependencies = append(r.Resolutions[0].Dependencies, d)
			case "policies":
				d.Policy.Follow = "none"
				r.Resolutions[0].Dependencies = append(r.Resolutions[0].Dependencies, d)
			case "captures":
				second := r.Resolutions[0]
				second.Capture = digest.FromString("other capture")
				second.Dependencies = []Dependency{d}
				r.Resolutions = append(r.Resolutions, second)
			case "subject loop":
				d.Ref = "source.example/charts/web:stable"
				d.Source = r.Source
				d.Destination = "target.example/alternate@" + digest.FromString("subject").String()
				r.Resolutions[0].Dependencies = []Dependency{d}
			}
			if _, err := Encode(r); err == nil {
				t.Fatal("accepted conflicting canonical destinations")
			}
			// Decode must enforce this too, independent of Encode's canonicalization.
			r.SchemaVersion = Version
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Decode(raw); err == nil {
				t.Fatal("decoded conflicting canonical destinations")
			}
		})
	}
	// Source repositories remain part of identity, even when their digests match.
	r := fixture()
	d := r.Resolutions[0].Dependencies[0]
	d.Ref = "other.example/images/web:stable"
	d.Source = "other.example/images/web@" + digest.FromString("child").String()
	d.Destination = "target.example/alternate@" + digest.FromString("child").String()
	r.Resolutions[0].Dependencies = append(r.Resolutions[0].Dependencies, d)
	if _, err := Encode(r); err != nil {
		t.Fatalf("conflated source repositories: %v", err)
	}
	// Self references with the subject's canonical location are valid.
	r = fixture()
	d.Ref = "source.example/charts/web:stable"
	d.Source = r.Source
	d.Destination = r.Destination
	r.Resolutions[0].Dependencies = append(r.Resolutions[0].Dependencies, d)
	if _, err := Encode(r); err != nil {
		t.Fatalf("rejected canonical self reference: %v", err)
	}
}
