// Package yamlutil validates the strict YAML subset used by authoring files.
package yamlutil

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

func Text(n *yaml.Node) (string, error) {
	if n == nil {
		return "", fmt.Errorf("expected a nonempty string")
	}
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" || strings.TrimSpace(n.Value) == "" || n.Value != strings.TrimSpace(n.Value) {
		return "", fmt.Errorf("line %d: expected a nonempty string without surrounding whitespace", n.Line)
	}
	return n.Value, nil
}

func Fields(n *yaml.Node, allowed ...string) (map[string]*yaml.Node, error) {
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: expected a mapping", n.Line)
	}
	m := map[string]*yaml.Node{}
	for i := 0; i < len(n.Content); i += 2 {
		key := n.Content[i].Value
		valid := false
		for _, a := range allowed {
			valid = valid || key == a
		}
		if !valid {
			return nil, fmt.Errorf("line %d: unknown field %q", n.Content[i].Line, key)
		}
		m[key] = n.Content[i+1]
	}
	return m, nil
}

func ValidateNodes(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return fmt.Errorf("line %d: YAML aliases and anchors are not supported", n.Line)
	}
	if n.Tag == "!!null" || (n.Tag != "" && n.Tag != "!!map" && n.Tag != "!!seq" && n.Tag != "!!str" && n.Kind != yaml.DocumentNode) {
		return fmt.Errorf("line %d: only strings, arrays, and mappings are allowed", n.Line)
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "<<" {
				return fmt.Errorf("line %d: mapping keys must be strings", key.Line)
			}
			if seen[key.Value] {
				return fmt.Errorf("line %d: duplicate key %q", key.Line, key.Value)
			}
			seen[key.Value] = true
		}
	}
	for _, child := range n.Content {
		if err := ValidateNodes(child); err != nil {
			return err
		}
	}
	return nil
}
