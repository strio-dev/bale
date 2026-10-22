package transport

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/destination"
	"oras.land/oras-go/v2/content"
)

func inspectionAttachment(t *testing.T, r *readRegistry, repo string, subject v1.Descriptor, context string) v1.Descriptor {
	t.Helper()
	record := destination.Record{SchemaVersion: 1, Bundle: repo + "@" + digest.FromString("bundle").String(), Source: "source.example/chart@" + subject.Digest.String(), Destination: repo + "@" + subject.Digest.String(), Resolutions: []destination.Resolution{{Capture: digest.FromString("capture"), Outcome: "selected", Declaration: &destination.Declaration{Digest: digest.FromString("declaration"), Context: context}, Dependencies: []destination.Dependency{}}}}
	desc, source, err := destination.Prepare(t.Context(), subject, record)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := content.FetchAll(t.Context(), source, desc)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, d := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
		data, err := content.FetchAll(t.Context(), source, d)
		if err != nil {
			t.Fatal(err)
		}
		put(t, r.store, d.MediaType, data)
	}
	put(t, r.store, desc.MediaType, raw)
	r.refs[subject.Digest.String()] = append(r.refs[subject.Digest.String()], desc)
	return desc
}
func TestDestinationInspectionRejectsInventoryCountBeforeFetching(t *testing.T) {
	r, repo, service := testRegistry(t)
	subject := object(t, r.store, "chart")
	r.tags["chart"] = subject
	attachment := inspectionAttachment(t, r, repo, subject, "production")
	r.refs[subject.Digest.String()] = []v1.Descriptor{attachment, attachment, attachment}
	result, err := service.InspectDestinationsWithOptions(t.Context(), repo+":chart", DestinationInspectionOptions{MaxMetadataBytes: 4 << 20, MaxReferrers: 2})
	if err == nil || !errors.Is(err, ErrUnsupported) || result != nil {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if r.headTags[attachment.Digest.String()] != 0 || r.blobReads != 0 {
		t.Fatal("downloaded attachments beyond inventory count limit")
	}
	// The original API remains unchanged and can inspect the same inventory.
	result, err = service.InspectDestinations(t.Context(), repo+":chart")
	if err != nil || len(result) != 1 {
		t.Fatalf("default inspection changed: %v %v", result, err)
	}
}
func TestDestinationInspectionRejectsPayloadBeforeFetching(t *testing.T) {
	r, repo, service := testRegistry(t)
	subject := object(t, r.store, "chart")
	r.tags["chart"] = subject
	attachment := inspectionAttachment(t, r, repo, subject, strings.Repeat("x", 100000))
	inventory, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: r.refs[subject.Digest.String()]})
	if err != nil {
		t.Fatal(err)
	}
	// Permit inventory, classification manifest, load manifest and config, but no
	// record payload. The oversized layer must be refused before its HTTP GET.
	budget := int64(len(inventory)+1) + attachment.Size*2 + 2
	result, err := service.InspectDestinationsWithOptions(t.Context(), repo+":chart", DestinationInspectionOptions{MaxMetadataBytes: budget, MaxReferrers: 8})
	if err == nil || !errors.Is(err, ErrUnsupported) || result != nil {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if r.blobReads != 1 {
		t.Fatalf("payload fetched despite exhausted budget: blob reads=%d", r.blobReads)
	}
}
func TestDestinationInspectionBudgetOptions(t *testing.T) {
	_, repo, service := testRegistry(t)
	for _, options := range []DestinationInspectionOptions{{MaxMetadataBytes: -1}, {MaxReferrers: -1}} {
		result, err := service.InspectDestinationsWithOptions(t.Context(), repo+":unavailable", options)
		if !errors.Is(err, ErrInvalid) || result != nil {
			t.Fatalf("result=%v err=%v", result, err)
		}
	}
}

func TestDestinationInspectionRejectsInventoryByteBudget(t *testing.T) {
	r, repo, service := testRegistry(t)
	subject := object(t, r.store, "chart")
	r.tags["chart"] = subject
	attachment := inspectionAttachment(t, r, repo, subject, "production")
	result, err := service.InspectDestinationsWithOptions(t.Context(), repo+":chart", DestinationInspectionOptions{MaxMetadataBytes: 10})
	if !errors.Is(err, ErrUnsupported) || result != nil {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if r.headTags[attachment.Digest.String()] != 0 || r.blobReads != 0 {
		t.Fatal("downloaded attachment after inventory exhausted byte budget")
	}
}

func TestDestinationInspectionBudgetReturnsNoPartialRecords(t *testing.T) {
	r, repo, service := testRegistry(t)
	subject := object(t, r.store, "chart")
	r.tags["chart"] = subject
	first := inspectionAttachment(t, r, repo, subject, strings.Repeat("a", 1000))
	second := inspectionAttachment(t, r, repo, subject, strings.Repeat("b", 1000))
	ordered := []v1.Descriptor{first, second}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Digest < ordered[j].Digest })
	raw, err := content.FetchAll(t.Context(), r.store, ordered[0])
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	inventory, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: r.refs[subject.Digest.String()]})
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(len(inventory)+1) + 2*(first.Size+second.Size) + 4 + manifest.Layers[0].Size
	result, err := service.InspectDestinationsWithOptions(t.Context(), repo+":chart", DestinationInspectionOptions{MaxMetadataBytes: budget, MaxReferrers: 8})
	if err == nil || !errors.Is(err, ErrUnsupported) || result != nil {
		t.Fatalf("returned partial records: result=%v err=%v", result, err)
	}
	if r.blobReads != 3 {
		t.Fatalf("expected first config/payload and second config only; reads=%d", r.blobReads)
	}
}
