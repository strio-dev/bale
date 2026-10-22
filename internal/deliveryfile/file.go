// Package deliveryfile defines editable, bundle-bound OCI publication paths.
package deliveryfile

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"

	digest "github.com/opencontainers/go-digest"
	"github.com/strio-dev/bale/internal/dependency"
	"github.com/strio-dev/bale/internal/yamlutil"
	"go.yaml.in/yaml/v3"
	"oras.land/oras-go/v2/registry"
)

// File binds path overrides to one immutable bundle. Omitted artifacts retain
// their default placement; this file never controls captured membership.
type File struct {
	Bundle    digest.Digest `json:"bundle" yaml:"bundle"`
	Artifacts []Artifact    `json:"artifacts" yaml:"artifacts"`
}

type Artifact struct {
	Ref  string `json:"ref" yaml:"ref"`
	Path string `json:"path" yaml:"path"`
}

// Normalize validates a defensive copy and orders artifacts by source identity.
func Normalize(file File) (File, error) {
	if err := file.Bundle.Validate(); err != nil {
		return File{}, fmt.Errorf("bundle: %w", err)
	}
	result := File{Bundle: file.Bundle, Artifacts: append([]Artifact{}, file.Artifacts...)}
	seen := map[string]bool{}
	for i, artifact := range result.Artifacts {
		if err := dependency.ValidateReference(artifact.Ref); err != nil {
			return File{}, fmt.Errorf("artifacts[%d].ref: %w", i, err)
		}
		ref, err := registry.ParseReference(artifact.Ref)
		if err != nil {
			return File{}, fmt.Errorf("artifacts[%d].ref: %w", i, err)
		}
		if err := ref.ValidateReferenceAsDigest(); err != nil || artifact.Ref != ref.Registry+"/"+ref.Repository+"@"+ref.Reference {
			return File{}, fmt.Errorf("artifacts[%d].ref: expected repository@digest", i)
		}
		if seen[artifact.Ref] {
			return File{}, fmt.Errorf("artifacts[%d]: duplicate artifact %s", i, artifact.Ref)
		}
		seen[artifact.Ref] = true
		if strings.TrimSpace(artifact.Path) != artifact.Path || artifact.Path == "" {
			return File{}, fmt.Errorf("artifacts[%d].path: expected a relative repository path", i)
		}
		if err := (registry.Reference{Repository: artifact.Path}).ValidateRepository(); err != nil {
			return File{}, fmt.Errorf("artifacts[%d].path: %w", i, err)
		}
	}
	sort.Slice(result.Artifacts, func(i, j int) bool { return result.Artifacts[i].Ref < result.Artifacts[j].Ref })
	return result, nil
}

// Encode produces deterministic YAML without changing the caller's file.
func Encode(file File) ([]byte, error) {
	normalized, err := Normalize(file)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(normalized); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// Decode accepts one strict YAML document, rejecting unknown fields, duplicate
// keys, aliases, nulls, coercions, duplicate identities and unsafe paths.
func Decode(data []byte) (File, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return File{}, fmt.Errorf("delivery file: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return File{}, fmt.Errorf("delivery file: expected exactly one YAML document")
	}
	if err := yamlutil.ValidateNodes(&document); err != nil {
		return File{}, fmt.Errorf("delivery file: %w", err)
	}
	if len(document.Content) != 1 {
		return File{}, fmt.Errorf("delivery file: expected a mapping")
	}
	fields, err := yamlutil.Fields(document.Content[0], "bundle", "artifacts")
	if err != nil {
		return File{}, err
	}
	value, err := yamlutil.Text(fields["bundle"])
	if err != nil {
		return File{}, fmt.Errorf("bundle: %w", err)
	}
	file := File{Bundle: digest.Digest(value), Artifacts: []Artifact{}}
	artifacts := fields["artifacts"]
	if artifacts == nil || artifacts.Kind != yaml.SequenceNode {
		return File{}, fmt.Errorf("artifacts: expected an array")
	}
	for i, node := range artifacts.Content {
		entry, err := yamlutil.Fields(node, "ref", "path")
		if err != nil {
			return File{}, fmt.Errorf("artifacts[%d]: %w", i, err)
		}
		ref, err := yamlutil.Text(entry["ref"])
		if err != nil {
			return File{}, fmt.Errorf("artifacts[%d].ref: %w", i, err)
		}
		path, err := yamlutil.Text(entry["path"])
		if err != nil {
			return File{}, fmt.Errorf("artifacts[%d].path: %w", i, err)
		}
		file.Artifacts = append(file.Artifacts, Artifact{Ref: ref, Path: path})
	}
	return Normalize(file)
}
