package relocate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/distribution/reference"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	bale "github.com/strio-dev/bale"
)

// MaxRecordsFileBytes bounds aggregate inspection metadata read from files or registries.
const MaxRecordsFileBytes = 4 << 20

// MaxInspectionReferrers bounds all attachment candidates inspected for one chart.
const MaxInspectionReferrers = 256

// DestinationInspector resolves a subject and reads its verified destination records.
type DestinationInspector interface {
	InspectDestinations(context.Context, string) ([]bale.InspectedDestination, error)
}

// Selection identifies one physical attachment and one selected dependency resolution.
type Selection struct {
	Chart      string
	Attachment v1.Descriptor
	Record     bale.DestinationRecord
	Resolution bale.DestinationResolution
	mapping    map[string]string
	inspection inspected
}

// Mappings returns a copy of normalized source-to-destination reference mappings.
func (s Selection) Mappings() map[string]string {
	out := make(map[string]string, len(s.mapping))
	for k, v := range s.mapping {
		out[k] = v
	}
	return out
}

// MarshalRecords returns the selected physical inspection envelope for an offline renderer.
func (s Selection) MarshalRecords() ([]byte, error) {
	if s.inspection.Repository == "" {
		return nil, fmt.Errorf("uninitialized selection")
	}
	return json.Marshal([]inspected{s.inspection})
}

// Select validates typed records and selects a single attachment and resolution.
func Select(records []bale.InspectedDestination, o Options) (Selection, error) {
	raw, err := json.Marshal(records)
	if err != nil {
		return Selection{}, err
	}
	return selectJSON(raw, o)
}

// Resolve inspects a reference once and pins selection to the returned registry subject.
func Resolve(ctx context.Context, inspector DestinationInspector, ref string, o Options) (Selection, error) {
	if inspector == nil {
		return Selection{}, fmt.Errorf("destination inspector is required")
	}
	if err := validateSelectors(o); err != nil {
		return Selection{}, err
	}
	named, err := reference.ParseNormalizedNamed(strings.TrimPrefix(ref, "oci://"))
	if err != nil {
		return Selection{}, fmt.Errorf("chart reference: %w", err)
	}
	if _, tag := named.(reference.Tagged); !tag {
		if _, pin := named.(reference.Digested); !pin {
			return Selection{}, fmt.Errorf("chart reference requires a tag or digest")
		}
	}
	repository := reference.TrimNamed(named).String()
	var records []bale.InspectedDestination
	if bounded, ok := inspector.(interface {
		InspectDestinationsWithOptions(context.Context, string, bale.DestinationInspectionOptions) ([]bale.InspectedDestination, error)
	}); ok {
		records, err = bounded.InspectDestinationsWithOptions(ctx, strings.TrimPrefix(ref, "oci://"), bale.DestinationInspectionOptions{MaxMetadataBytes: MaxRecordsFileBytes, MaxReferrers: MaxInspectionReferrers})
	} else {
		records, err = inspector.InspectDestinations(ctx, strings.TrimPrefix(ref, "oci://"))
	}
	if err != nil {
		return Selection{}, err
	}
	if len(records) == 0 {
		return Selection{}, fmt.Errorf("missing destination records")
	}
	subject := records[0].Subject
	for _, r := range records {
		if r.Repository != repository || r.Subject.Digest != subject.Digest || r.Subject.Size != subject.Size || r.Subject.MediaType != subject.MediaType {
			return Selection{}, fmt.Errorf("incoherent inspection subjects")
		}
	}
	chart := repository + "@" + subject.Digest.String()
	if supplied, ok := named.(reference.Digested); ok && supplied.Digest() != subject.Digest {
		return Selection{}, fmt.Errorf("resolved chart digest mismatch")
	}
	if o.Chart != "" && o.Chart != chart {
		return Selection{}, fmt.Errorf("chart selector differs from resolved subject")
	}
	o.Chart = chart
	return Select(records, o)
}

// strictJSON rejects duplicate keys and null before struct decoding can discard them.
func strictJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var value func() error
	value = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		if t == nil {
			return fmt.Errorf("null is not allowed")
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return fmt.Errorf("duplicate or invalid JSON key %q", k)
				}
				seen[s] = true
				if err := value(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}

type inspected struct {
	Repository string          `json:"repository"`
	Subject    v1.Descriptor   `json:"subject"`
	Descriptor v1.Descriptor   `json:"descriptor"`
	Record     json.RawMessage `json:"record"`
	Local      *bool           `json:"local"`
}

func descriptor(d v1.Descriptor) error {
	if err := d.Digest.Validate(); err != nil {
		return err
	}
	if d.Size <= 0 || d.MediaType == "" {
		return fmt.Errorf("descriptor requires mediaType and positive size")
	}
	return nil
}
func selectJSON(raw []byte, o Options) (Selection, error) {
	if err := validateOptions(o); err != nil {
		return Selection{}, err
	}
	if len(raw) > MaxRecordsFileBytes {
		return Selection{}, fmt.Errorf("records exceed %d bytes", MaxRecordsFileBytes)
	}
	if err := strictJSON(raw); err != nil {
		return Selection{}, err
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		return Selection{}, fmt.Errorf("records must be an array")
	}
	var envelopes []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelopes); err != nil {
		return Selection{}, err
	}
	for _, envelope := range envelopes {
		for _, name := range []string{"repository", "subject", "descriptor", "record", "local"} {
			if _, ok := envelope[name]; !ok {
				return Selection{}, fmt.Errorf("inspection requires %s field", name)
			}
		}
		if len(envelope) != 5 {
			return Selection{}, fmt.Errorf("unknown inspection field")
		}
	}
	var entries []inspected
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&entries); err != nil {
		return Selection{}, err
	}
	records := map[string]inspected{}
	for _, e := range entries {
		r, err := bale.DecodeDestinationRecord(e.Record)
		if err != nil {
			return Selection{}, err
		}
		if e.Local == nil {
			return Selection{}, fmt.Errorf("inspection requires local field")
		}
		if err := descriptor(e.Subject); err != nil {
			return Selection{}, fmt.Errorf("subject: %w", err)
		}
		if err := descriptor(e.Descriptor); err != nil {
			return Selection{}, fmt.Errorf("descriptor: %w", err)
		}
		if e.Descriptor.ArtifactType != "application/vnd.bale.destination.v1+json" {
			return Selection{}, fmt.Errorf("unexpected destination artifactType")
		}
		subject := e.Repository + "@" + e.Subject.Digest.String()
		if err := pinned(subject); err != nil {
			return Selection{}, fmt.Errorf("inspection repository: %w", err)
		}
		_, sourceDigest, _ := strings.Cut(r.Source, "@")
		if sourceDigest != e.Subject.Digest.String() {
			return Selection{}, fmt.Errorf("record subject digest mismatch")
		}
		// Recompute locality; the untrusted serialized local flag is informational.
		if subject != o.Chart || r.Destination != subject {
			continue
		}
		if o.Bundle != "" && r.Bundle != o.Bundle || o.Source != "" && r.Source != o.Source {
			continue
		}
		canonical, _ := bale.EncodeDestinationRecord(r)
		records[e.Descriptor.Digest.String()+"/"+string(canonical)] = e
	}
	if len(records) != 1 {
		return Selection{}, fmt.Errorf("ambiguous or missing local destination record: found %d; select bundle/source explicitly", len(records))
	}
	var resolutions []bale.DestinationResolution
	var selected inspected
	for _, entry := range records {
		selected = entry
		r, _ := bale.DecodeDestinationRecord(entry.Record)
		for _, s := range r.Resolutions {
			if s.Outcome != "selected" {
				continue
			}
			if o.Capture != "" && s.Capture.String() != o.Capture {
				continue
			}
			if o.Declaration != "" && s.Declaration.Digest.String() != o.Declaration {
				continue
			}
			if o.Context != "" && s.Declaration.Context != o.Context {
				continue
			}
			resolutions = append(resolutions, s)
		}
	}
	if len(resolutions) != 1 {
		return Selection{}, fmt.Errorf("ambiguous or missing selected dependency resolution: found %d; select capture/declaration/context explicitly", len(resolutions))
	}
	mapping := map[string]string{}
	for _, dep := range resolutions[0].Dependencies {
		for _, alias := range []string{dep.Ref, dep.Source, dep.Destination} {
			key, err := normalize(alias)
			if err != nil {
				return Selection{}, err
			}
			if previous, ok := mapping[key]; ok && previous != dep.Destination {
				return Selection{}, fmt.Errorf("conflicting normalized image mapping %q", key)
			}
			mapping[key] = dep.Destination
		}
	}
	record, _ := bale.DecodeDestinationRecord(selected.Record)
	return Selection{Chart: o.Chart, Attachment: selected.Descriptor, Record: record, Resolution: resolutions[0], mapping: mapping, inspection: selected}, nil
}
