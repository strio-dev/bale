// Package dependency defines generic OCI dependency inclusion declarations.
package dependency

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	digest "github.com/opencontainers/go-digest"
	"github.com/strio-dev/bale/internal/jsonutil"
	"oras.land/oras-go/v2/registry"
)

const (
	ArtifactType = "application/vnd.bale.dependencies.v1+json"
	Version      = 1
	// MaxBytes bounds both declaration payloads and their OCI manifest.
	MaxBytes int64 = 4 << 20
)

type Declaration struct {
	SchemaVersion int         `json:"schemaVersion"`
	Context       string      `json:"context,omitempty"`
	Dependencies  []Reference `json:"dependencies"`
}
type Reference struct {
	Ref    string    `json:"ref"`
	Select *Selector `json:"select,omitempty"`
	Follow string    `json:"follow,omitempty"`
}
type Selector struct {
	Digest  digest.Digest `json:"digest,omitempty"`
	Context string        `json:"context,omitempty"`
}
type Policy struct {
	Follow string    `json:"follow"`
	Select *Selector `json:"select,omitempty"`
}

// ValidateReference requires an explicit registry and exactly one tag or digest.
func ValidateReference(value string) error {
	ref, err := registry.ParseReference(value)
	if err != nil {
		return err
	}
	if ref.Registry != "localhost" && !strings.ContainsAny(ref.Registry, ".:") {
		return fmt.Errorf("explicit registry host is required")
	}
	if ref.Reference == "" {
		return fmt.Errorf("reference needs an explicit tag or digest")
	}
	path := strings.SplitN(value, "/", 2)[1]
	if at := strings.IndexByte(path, '@'); at >= 0 && strings.Contains(path[:at], ":") {
		return fmt.Errorf("tag and digest cannot be combined")
	}
	return nil
}

// NormalizePolicy applies the automatic-following default and copies selectors.
func NormalizePolicy(p Policy) (Policy, error) {
	if p.Follow == "" {
		p.Follow = "auto"
	}
	if p.Follow != "auto" && p.Follow != "none" {
		return Policy{}, fmt.Errorf("dependencies.follow must be auto or none")
	}
	if p.Select != nil {
		if p.Follow == "none" {
			return Policy{}, fmt.Errorf("dependencies.select cannot be combined with follow: none")
		}
		s := *p.Select
		if (s.Digest == "") == (s.Context == "") {
			return Policy{}, fmt.Errorf("dependencies.select requires exactly one digest or context")
		}
		if s.Digest != "" {
			if err := s.Digest.Validate(); err != nil {
				return Policy{}, fmt.Errorf("dependencies.select: %w", err)
			}
		}
		if s.Context != "" && strings.TrimSpace(s.Context) != s.Context {
			return Policy{}, fmt.Errorf("dependencies.select.context cannot have surrounding whitespace")
		}
		p.Select = &s
	}
	return p, nil
}

func normalize(d Declaration) (Declaration, error) {
	if d.SchemaVersion != Version {
		return Declaration{}, fmt.Errorf("unsupported dependency schemaVersion %d", d.SchemaVersion)
	}
	if d.Context != "" && strings.TrimSpace(d.Context) != d.Context {
		return Declaration{}, fmt.Errorf("context cannot have surrounding whitespace")
	}
	refs := make(map[string]Reference, len(d.Dependencies))
	for i, r := range d.Dependencies {
		if err := ValidateReference(r.Ref); err != nil {
			return Declaration{}, fmt.Errorf("dependencies[%d]: %w", i, err)
		}
		p, err := NormalizePolicy(Policy{Follow: r.Follow, Select: r.Select})
		if err != nil {
			return Declaration{}, fmt.Errorf("dependencies[%d]: %w", i, err)
		}
		r.Follow, r.Select = p.Follow, p.Select
		if r.Follow == "auto" {
			r.Follow = ""
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return Declaration{}, err
		}
		refs[string(raw)] = r
	}
	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	d.Dependencies = make([]Reference, 0, len(keys))
	for _, key := range keys {
		d.Dependencies = append(d.Dependencies, refs[key])
	}
	return d, nil
}

// Encode produces deterministic JSON, defaulting the version and empty array.
func Encode(d Declaration) ([]byte, error) {
	if d.SchemaVersion == 0 {
		d.SchemaVersion = Version
	}
	normalized, err := normalize(d)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > MaxBytes {
		return nil, fmt.Errorf("dependency declaration exceeds %d bytes", MaxBytes)
	}
	return raw, nil
}

// Decode rejects unknown fields, duplicate keys, nulls and trailing values.
func Decode(raw []byte) (Declaration, error) {
	if int64(len(raw)) > MaxBytes {
		return Declaration{}, fmt.Errorf("dependency declaration exceeds %d bytes", MaxBytes)
	}
	if err := jsonutil.Validate(raw, true); err != nil {
		return Declaration{}, fmt.Errorf("dependency declaration: %w", err)
	}
	if err := validateFields(raw); err != nil {
		return Declaration{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d Declaration
	if err := dec.Decode(&d); err != nil {
		return Declaration{}, err
	}
	if d.Dependencies == nil {
		return Declaration{}, fmt.Errorf("dependencies must be an array")
	}
	return normalize(d)
}

func object(raw []byte, allowed ...string) (map[string]json.RawMessage, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return nil, fmt.Errorf("expected an object")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for key := range m {
		found := false
		for _, a := range allowed {
			found = found || key == a
		}
		if !found {
			return nil, fmt.Errorf("unknown field %q", key)
		}
	}
	return m, nil
}

func nonempty(m map[string]json.RawMessage, key string) error {
	if raw, ok := m[key]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		if s == "" {
			return fmt.Errorf("%s must be nonempty when present", key)
		}
	}
	return nil
}

func validateFields(raw []byte) error {
	m, err := object(raw, "schemaVersion", "context", "dependencies")
	if err != nil {
		return err
	}
	if err := nonempty(m, "context"); err != nil {
		return err
	}
	var refs []json.RawMessage
	if err := json.Unmarshal(m["dependencies"], &refs); err != nil {
		return err
	}
	for _, raw := range refs {
		r, err := object(raw, "ref", "select", "follow")
		if err != nil {
			return err
		}
		if err := nonempty(r, "follow"); err != nil {
			return err
		}
		if raw, ok := r["select"]; ok {
			s, err := object(raw, "digest", "context")
			if err != nil {
				return err
			}
			for _, key := range []string{"digest", "context"} {
				if err := nonempty(s, key); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
