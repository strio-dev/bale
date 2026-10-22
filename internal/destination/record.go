// Package destination describes confirmed artifact delivery locations.
package destination

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	digest "github.com/opencontainers/go-digest"
	"github.com/strio-dev/bale/internal/dependency"
	"github.com/strio-dev/bale/internal/jsonutil"
	"oras.land/oras-go/v2/registry"
)

const (
	ArtifactType       = "application/vnd.bale.destination.v1+json"
	Version            = 1
	MaxBytes     int64 = 4 << 20
)

type Record struct {
	SchemaVersion int          `json:"schemaVersion"`
	Bundle        string       `json:"bundle"`
	Source        string       `json:"source"`
	Destination   string       `json:"destination"`
	Resolutions   []Resolution `json:"resolutions"`
}
type Resolution struct {
	Capture      digest.Digest     `json:"capture"`
	Policy       dependency.Policy `json:"policy"`
	Outcome      string            `json:"outcome"`
	Declaration  *Declaration      `json:"declaration,omitempty"`
	Dependencies []Dependency      `json:"dependencies"`
}
type Declaration struct {
	Digest  digest.Digest `json:"digest"`
	Context string        `json:"context,omitempty"`
}
type Dependency struct {
	Ref         string            `json:"ref"`
	Policy      dependency.Policy `json:"policy"`
	Source      string            `json:"source"`
	Destination string            `json:"destination"`
}

func pinned(value string) (registry.Reference, error) {
	if err := dependency.ValidateReference(value); err != nil {
		return registry.Reference{}, err
	}
	ref, err := registry.ParseReference(value)
	if err != nil {
		return ref, err
	}
	if _, err := digest.Parse(ref.Reference); err != nil {
		return ref, fmt.Errorf("reference must be digest pinned: %s", value)
	}
	return ref, nil
}
func locationPair(source, destination string) (registry.Reference, error) {
	src, err := pinned(source)
	if err != nil {
		return src, err
	}
	dst, err := pinned(destination)
	if err != nil {
		return src, err
	}
	if src.Reference != dst.Reference {
		return src, fmt.Errorf("source and destination digests differ")
	}
	return src, nil
}
func normalize(r Record) (Record, error) {
	if r.SchemaVersion != Version {
		return Record{}, fmt.Errorf("unsupported destination schemaVersion %d", r.SchemaVersion)
	}
	if _, err := pinned(r.Bundle); err != nil {
		return Record{}, fmt.Errorf("bundle: %w", err)
	}
	if _, err := locationPair(r.Source, r.Destination); err != nil {
		return Record{}, err
	}
	resolutions := map[string]Resolution{}
	references := map[string]string{}
	// A source coordinate has one canonical destination across aliases, policies
	// and nested captures, including dependencies that point back to the subject.
	destinations := map[string]string{r.Source: r.Destination}
	for _, resolution := range r.Resolutions {
		if err := resolution.Capture.Validate(); err != nil {
			return Record{}, fmt.Errorf("capture: %w", err)
		}
		p, err := dependency.NormalizePolicy(resolution.Policy)
		if err != nil {
			return Record{}, err
		}
		resolution.Policy = p
		if resolution.Declaration != nil {
			d := *resolution.Declaration
			resolution.Declaration = &d
			if err := d.Digest.Validate(); err != nil {
				return Record{}, err
			}
			if strings.TrimSpace(d.Context) != d.Context {
				return Record{}, fmt.Errorf("declaration context cannot have surrounding whitespace")
			}
		}
		switch resolution.Outcome {
		case "selected":
			if p.Follow == "none" || resolution.Declaration == nil {
				return Record{}, fmt.Errorf("selected resolution requires enabled policy and declaration")
			}
			if p.Select != nil && ((p.Select.Digest != "" && p.Select.Digest != resolution.Declaration.Digest) || (p.Select.Context != "" && p.Select.Context != resolution.Declaration.Context)) {
				return Record{}, fmt.Errorf("declaration differs from policy selector")
			}
		case "absent":
			if p.Follow == "none" || p.Select != nil || resolution.Declaration != nil || len(resolution.Dependencies) != 0 {
				return Record{}, fmt.Errorf("invalid absent resolution")
			}
		case "disabled":
			if p.Follow != "none" || resolution.Declaration != nil || len(resolution.Dependencies) != 0 {
				return Record{}, fmt.Errorf("invalid disabled resolution")
			}
		default:
			return Record{}, fmt.Errorf("invalid resolution outcome %q", resolution.Outcome)
		}
		dependencies := map[string]Dependency{}
		for _, d := range resolution.Dependencies {
			if err := dependency.ValidateReference(d.Ref); err != nil {
				return Record{}, err
			}
			p, err := dependency.NormalizePolicy(d.Policy)
			if err != nil {
				return Record{}, err
			}
			d.Policy = p
			src, err := locationPair(d.Source, d.Destination)
			if err != nil {
				return Record{}, err
			}
			if previous, ok := destinations[d.Source]; ok && previous != d.Destination {
				return Record{}, fmt.Errorf("source has conflicting canonical destinations")
			}
			destinations[d.Source] = d.Destination
			ref, _ := registry.ParseReference(d.Ref)
			if ref.Registry != src.Registry || ref.Repository != src.Repository {
				return Record{}, fmt.Errorf("dependency source repository differs from original reference")
			}
			if original, err := digest.Parse(ref.Reference); err == nil && original.String() != src.Reference {
				return Record{}, fmt.Errorf("digest-pinned dependency changed digest")
			}
			referenceKey := resolution.Capture.String() + "/" + d.Ref
			if previous, ok := references[referenceKey]; ok && previous != d.Source {
				return Record{}, fmt.Errorf("dependency reference resolved inconsistently")
			}
			references[referenceKey] = d.Source
			keyRaw, _ := json.Marshal(struct {
				Ref    string
				Policy dependency.Policy
			}{d.Ref, d.Policy})
			key := string(keyRaw)
			if previous, ok := dependencies[key]; ok {
				a, _ := json.Marshal(previous)
				b, _ := json.Marshal(d)
				if !bytes.Equal(a, b) {
					return Record{}, fmt.Errorf("conflicting dependency mapping")
				}
			}
			dependencies[key] = d
		}
		keys := make([]string, 0, len(dependencies))
		for key := range dependencies {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		resolution.Dependencies = make([]Dependency, 0, len(keys))
		for _, key := range keys {
			resolution.Dependencies = append(resolution.Dependencies, dependencies[key])
		}
		policyRaw, _ := json.Marshal(p)
		key := resolution.Capture.String() + "/" + string(policyRaw)
		if previous, ok := resolutions[key]; ok {
			a, _ := json.Marshal(previous)
			b, _ := json.Marshal(resolution)
			if !bytes.Equal(a, b) {
				return Record{}, fmt.Errorf("conflicting capture resolution")
			}
		}
		resolutions[key] = resolution
	}
	keys := make([]string, 0, len(resolutions))
	for key := range resolutions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	r.Resolutions = make([]Resolution, 0, len(keys))
	for _, key := range keys {
		r.Resolutions = append(r.Resolutions, resolutions[key])
	}
	return r, nil
}

// Encode validates and deterministically encodes records, defaulting the version.
func Encode(r Record) ([]byte, error) {
	if r.SchemaVersion == 0 {
		r.SchemaVersion = Version
	}
	r, err := normalize(r)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > MaxBytes {
		return nil, fmt.Errorf("destination record exceeds %d bytes", MaxBytes)
	}
	return raw, nil
}

// Decode rejects ambiguous JSON and validates references without resolving them.
func Decode(raw []byte) (Record, error) {
	if int64(len(raw)) > MaxBytes {
		return Record{}, fmt.Errorf("destination record exceeds %d bytes", MaxBytes)
	}
	if err := jsonutil.Validate(raw, true); err != nil {
		return Record{}, err
	}
	if err := validateFields(raw); err != nil {
		return Record{}, err
	}
	var r Record
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Record{}, err
	}
	return normalize(r)
}

func object(raw []byte, required []string, optional ...string) (map[string]json.RawMessage, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return nil, fmt.Errorf("expected object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, key := range required {
		allowed[key] = true
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("missing %s", key)
		}
	}
	for _, key := range optional {
		allowed[key] = true
	}
	for key := range fields {
		if !allowed[key] {
			return nil, fmt.Errorf("unknown field %q", key)
		}
	}
	for _, key := range optional {
		if value, ok := fields[key]; ok && bytes.Equal(value, []byte(`""`)) {
			return nil, fmt.Errorf("%s must be nonempty when present", key)
		}
	}
	return fields, nil
}
func array(raw []byte) ([]json.RawMessage, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		return nil, fmt.Errorf("expected array")
	}
	var items []json.RawMessage
	err := json.Unmarshal(raw, &items)
	return items, err
}
func policyFields(raw []byte) error {
	fields, err := object(raw, nil, "follow", "select")
	if err != nil {
		return err
	}
	if value, ok := fields["select"]; ok {
		_, err = object(value, nil, "digest", "context")
	}
	return err
}
func validateFields(raw []byte) error {
	fields, err := object(raw, []string{"schemaVersion", "bundle", "source", "destination", "resolutions"})
	if err != nil {
		return err
	}
	items, err := array(fields["resolutions"])
	if err != nil {
		return err
	}
	for _, item := range items {
		resolution, err := object(item, []string{"capture", "policy", "outcome", "dependencies"}, "declaration")
		if err != nil {
			return err
		}
		if err := policyFields(resolution["policy"]); err != nil {
			return err
		}
		if value, ok := resolution["declaration"]; ok {
			if _, err := object(value, []string{"digest"}, "context"); err != nil {
				return err
			}
		}
		deps, err := array(resolution["dependencies"])
		if err != nil {
			return err
		}
		for _, item := range deps {
			d, err := object(item, []string{"ref", "policy", "source", "destination"})
			if err != nil {
				return err
			}
			if err := policyFields(d["policy"]); err != nil {
				return err
			}
		}
	}
	return nil
}
