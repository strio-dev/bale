package relocate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	bale "github.com/strio-dev/bale"
)

type inspectorFunc func(context.Context, string) ([]bale.InspectedDestination, error)

func (f inspectorFunc) InspectDestinations(ctx context.Context, r string) ([]bale.InspectedDestination, error) {
	return f(ctx, r)
}
func typedInventory(t *testing.T) []bale.InspectedDestination {
	t.Helper()
	var r []bale.InspectedDestination
	if err := json.Unmarshal(inventory(t), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResolvePinsSingleInspection(t *testing.T) {
	records := typedInventory(t)
	calls := 0
	s, err := Resolve(context.Background(), inspectorFunc(func(_ context.Context, ref string) ([]bale.InspectedDestination, error) {
		calls++
		if ref != "target.example/charts/demo:latest" {
			t.Fatalf("unexpected reference %s", ref)
		}
		return records, nil
	}), "oci://target.example/charts/demo:latest", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || s.Chart != chart {
		t.Fatalf("calls=%d chart=%s", calls, s.Chart)
	}
	mappings := s.Mappings()
	mappings["docker.io/library/nginx:latest"] = "changed"
	if s.Mappings()["docker.io/library/nginx:latest"] != image {
		t.Fatal("mapping exposed mutable state")
	}
	raw, err := s.MarshalRecords()
	if err != nil {
		t.Fatal(err)
	}
	a, err := Relocate([]byte(pod), raw, Options{Chart: s.Chart})
	if err != nil {
		t.Fatal(err)
	}
	b, err := RelocateSelection([]byte(pod), s)
	if err != nil || string(a) != string(b) {
		t.Fatalf("selection differs: %v", err)
	}
}
func TestResolveRejectsInvalidSelectorsBeforeLookup(t *testing.T) {
	calls := 0
	_, err := Resolve(context.Background(), inspectorFunc(func(context.Context, string) ([]bale.InspectedDestination, error) {
		calls++
		return typedInventory(t), nil
	}), "target.example/charts/demo:latest", Options{Bundle: "not-pinned"})
	if err == nil || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
func TestTypedSelectionValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func([]bale.InspectedDestination) []bale.InspectedDestination
	}{
		{"bad record", func(r []bale.InspectedDestination) []bale.InspectedDestination { r[0].Record.Bundle = "bad"; return r }},
		{"bad descriptor", func(r []bale.InspectedDestination) []bale.InspectedDestination { r[0].Descriptor.Size = 0; return r }},
		{"different physical attachment", func(r []bale.InspectedDestination) []bale.InspectedDestination {
			other := r[0]
			other.Descriptor.Digest = digest.FromString("another")
			return append(r, other)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Select(tc.change(typedInventory(t)), Options{Chart: chart}); err == nil {
				t.Fatal("accepted invalid or ambiguous records")
			}
		})
	}
	records := typedInventory(t)
	records[0].Local = false
	if _, err := Select(records, Options{Chart: chart}); err != nil {
		t.Fatal("trusted serialized locality", err)
	}
}
func TestResolveCoherentSubject(t *testing.T) {
	records := typedInventory(t)
	other := records[0]
	other.Subject.Digest = digest.FromString("other")
	records = append(records, other)
	if _, err := Resolve(context.Background(), inspectorFunc(func(context.Context, string) ([]bale.InspectedDestination, error) { return records, nil }), "target.example/charts/demo:latest", Options{}); err == nil {
		t.Fatal("incoherent subjects accepted")
	}
}
func TestRecordsFileLimitsAndStrictDecode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	write := func(raw []byte) {
		t.Helper()
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw := append(inventory(t), []byte(strings.Repeat(" ", MaxRecordsBytes))...)
	write(raw)
	o, actual, err := ParseArgs([]string{"--chart", chart, "--records-file", path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Relocate([]byte(pod), actual, o); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseArgs([]string{"--chart", chart, "--records-json", string(raw)}); err == nil {
		t.Fatal("oversized argument accepted")
	}
	if _, _, err := ParseArgs([]string{"--chart", chart, "--records-json", string(inventory(t)), "--records-file", path}); err == nil {
		t.Fatal("two inputs accepted")
	}
	write([]byte(strings.Repeat(" ", MaxRecordsFileBytes+1)))
	if _, _, err := ParseArgs([]string{"--chart", chart, "--records-file", path}); err == nil {
		t.Fatal("oversized file accepted")
	}
	write([]byte(`[{"repository":"x","repository":"y"}]`))
	o, actual, err = ParseArgs([]string{"--chart", chart, "--records-file", path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Relocate([]byte(pod), actual, o); err == nil {
		t.Fatal("duplicate keys accepted from file")
	}
}

type boundedInspector struct {
	records            []bale.InspectedDestination
	bounded, unbounded int
	options            bale.DestinationInspectionOptions
}

func (i *boundedInspector) InspectDestinations(context.Context, string) ([]bale.InspectedDestination, error) {
	i.unbounded++
	return i.records, nil
}
func (i *boundedInspector) InspectDestinationsWithOptions(_ context.Context, _ string, o bale.DestinationInspectionOptions) ([]bale.InspectedDestination, error) {
	i.bounded++
	i.options = o
	return i.records, nil
}
func TestResolveUsesBoundedSDKInspection(t *testing.T) {
	inspector := &boundedInspector{records: typedInventory(t)}
	if _, err := Resolve(t.Context(), inspector, "target.example/charts/demo:latest", Options{}); err != nil {
		t.Fatal(err)
	}
	if inspector.bounded != 1 || inspector.unbounded != 0 || inspector.options.MaxMetadataBytes != MaxRecordsFileBytes || inspector.options.MaxReferrers != MaxInspectionReferrers {
		t.Fatalf("unbounded registry inspection: %+v", inspector)
	}
}
