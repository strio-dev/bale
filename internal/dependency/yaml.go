package dependency

import (
	"bytes"
	"fmt"
	"io"

	digest "github.com/opencontainers/go-digest"
	"github.com/strio-dev/bale/internal/yamlutil"
	"go.yaml.in/yaml/v3"
)

// ParseYAML reads user-authored selectors without resolving references. Registry
// declarations remain canonical JSON with the current implicit schema version.
func ParseYAML(data []byte) (Declaration, error) {
	if int64(len(data)) > MaxBytes {
		return Declaration{}, fmt.Errorf("dependency YAML exceeds %d bytes", MaxBytes)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return Declaration{}, fmt.Errorf("dependency YAML: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return Declaration{}, fmt.Errorf("dependency YAML: expected exactly one document")
	}
	if err := yamlutil.ValidateNodes(&document); err != nil {
		return Declaration{}, err
	}
	if len(document.Content) != 1 {
		return Declaration{}, fmt.Errorf("dependency YAML: expected a mapping")
	}
	fields, err := yamlutil.Fields(document.Content[0], "context", "dependencies")
	if err != nil {
		return Declaration{}, err
	}
	declaration := Declaration{SchemaVersion: Version, Dependencies: []Reference{}}
	if node := fields["context"]; node != nil {
		declaration.Context, err = yamlutil.Text(node)
		if err != nil {
			return Declaration{}, fmt.Errorf("context: %w", err)
		}
	}
	dependencies := fields["dependencies"]
	if dependencies == nil || dependencies.Kind != yaml.SequenceNode {
		return Declaration{}, fmt.Errorf("dependencies: expected an array")
	}
	for i, node := range dependencies.Content {
		entry, err := parseYAMLReference(node)
		if err != nil {
			return Declaration{}, fmt.Errorf("dependencies[%d]: %w", i, err)
		}
		declaration.Dependencies = append(declaration.Dependencies, entry)
	}
	// Reuse the registry codec's normalization, selector validation, deduplication
	// and JSON size limit instead of introducing different authoring semantics.
	raw, err := Encode(declaration)
	if err != nil {
		return Declaration{}, err
	}
	return Decode(raw)
}

func parseYAMLReference(node *yaml.Node) (Reference, error) {
	fields, err := yamlutil.Fields(node, "ref", "select", "follow")
	if err != nil {
		return Reference{}, err
	}
	ref, err := yamlutil.Text(fields["ref"])
	if err != nil {
		return Reference{}, fmt.Errorf("ref: %w", err)
	}
	entry := Reference{Ref: ref}
	if node := fields["follow"]; node != nil {
		entry.Follow, err = yamlutil.Text(node)
		if err != nil {
			return Reference{}, fmt.Errorf("follow: %w", err)
		}
	}
	if node := fields["select"]; node != nil {
		fields, err := yamlutil.Fields(node, "digest", "context")
		if err != nil {
			return Reference{}, err
		}
		entry.Select = &Selector{}
		if node := fields["digest"]; node != nil {
			value, err := yamlutil.Text(node)
			if err != nil {
				return Reference{}, err
			}
			entry.Select.Digest = digest.Digest(value)
		}
		if node := fields["context"]; node != nil {
			entry.Select.Context, err = yamlutil.Text(node)
			if err != nil {
				return Reference{}, err
			}
		}
	}
	return entry, nil
}
