package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/strio-dev/bale/internal/dependency"
	"github.com/strio-dev/bale/internal/jsonutil"
	"github.com/strio-dev/bale/internal/yamlutil"
	"go.yaml.in/yaml/v3"
)

const MaxPluginInputBytes = 8 << 20

// Plugin selects a portable OCI discovery plugin and its literal JSON input.
// Blobs binds top-level input fields to selected artifact layers by media type.
type Plugin struct {
	Ref   string                `json:"ref"`
	Input json.RawMessage       `json:"input"`
	Blobs map[string]PluginBlob `json:"blobs,omitempty"`
}
type PluginBlob struct {
	MediaType string `json:"mediaType"`
}

func parsePlugin(n *yaml.Node) (*Plugin, error) {
	fields, err := yamlutil.Fields(n, "ref", "input", "blobs")
	if err != nil {
		return nil, err
	}
	p := &Plugin{Input: json.RawMessage("{}")}
	p.Ref, err = yamlutil.Text(fields["ref"])
	if err != nil {
		return nil, err
	}
	if input := fields["input"]; input != nil {
		if input.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("plugin.input must be an object")
		}
		value, err := pluginJSON(input)
		if err != nil {
			return nil, err
		}
		p.Input, err = json.Marshal(value)
		if err != nil {
			return nil, err
		}
	}
	if blobs := fields["blobs"]; blobs != nil {
		if blobs.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("plugin.blobs must be a mapping")
		}
		p.Blobs = map[string]PluginBlob{}
		for i := 0; i < len(blobs.Content); i += 2 {
			fields, err := yamlutil.Fields(blobs.Content[i+1], "mediaType")
			if err != nil {
				return nil, err
			}
			mediaType, err := yamlutil.Text(fields["mediaType"])
			if err != nil {
				return nil, err
			}
			p.Blobs[blobs.Content[i].Value] = PluginBlob{MediaType: mediaType}
		}
	}
	return p, nil
}

func validatePlugin(p Plugin, policy dependency.Policy) error {
	if err := dependency.ValidateReference(p.Ref); err != nil {
		return err
	}
	if policy.Follow == "none" || policy.Select != nil {
		return fmt.Errorf("cannot combine plugin with dependencies.follow: none or dependencies.select")
	}
	if len(p.Input) > MaxPluginInputBytes {
		return fmt.Errorf("input exceeds %d bytes", MaxPluginInputBytes)
	}
	if err := jsonutil.Validate(p.Input, false); err != nil {
		return fmt.Errorf("input: %w", err)
	}
	var fields map[string]json.RawMessage
	if len(p.Input) == 0 || json.Unmarshal(p.Input, &fields) != nil || fields == nil {
		return fmt.Errorf("input must be a JSON object")
	}
	for key, blob := range p.Blobs {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key {
			return fmt.Errorf("blob input field must be nonempty without surrounding whitespace")
		}
		if strings.TrimSpace(blob.MediaType) == "" || strings.TrimSpace(blob.MediaType) != blob.MediaType {
			return fmt.Errorf("blob mediaType must be nonempty without surrounding whitespace")
		}
		if _, ok := fields[key]; ok {
			return fmt.Errorf("blob field %q conflicts with literal input", key)
		}
	}
	return nil
}

// Only plugin.input accepts JSON scalars. Keep the existing strict authoring
// subset everywhere else, including unknown fields, aliases and duplicate keys.
func validateRecipeNodes(doc *yaml.Node) error {
	var copyNode func(*yaml.Node, []string) (*yaml.Node, error)
	copyNode = func(n *yaml.Node, path []string) (*yaml.Node, error) {
		out := *n
		if len(path) == 4 && path[0] == "include" && path[2] == "plugin" && path[3] == "input" {
			if _, err := pluginJSON(n); err != nil {
				return nil, err
			}
			out.Kind = yaml.MappingNode
			out.Tag = "!!map"
			out.Content = nil
			return &out, nil
		}
		out.Content = nil
		for i, child := range n.Content {
			next := path
			if n.Kind == yaml.MappingNode && i%2 == 1 {
				next = append(append([]string{}, path...), n.Content[i-1].Value)
			}
			if n.Kind == yaml.SequenceNode {
				next = append(append([]string{}, path...), "[]")
			}
			cloned, err := copyNode(child, next)
			if err != nil {
				return nil, err
			}
			out.Content = append(out.Content, cloned)
		}
		return &out, nil
	}
	copy, err := copyNode(doc, nil)
	if err != nil {
		return err
	}
	return yamlutil.ValidateNodes(copy)
}

func pluginJSON(n *yaml.Node) (any, error) {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return nil, fmt.Errorf("plugin.input: YAML aliases and anchors are not supported")
	}
	switch n.Kind {
	case yaml.MappingNode:
		if n.Tag != "!!map" {
			return nil, fmt.Errorf("plugin.input: unsupported YAML mapping tag")
		}
		out := map[string]any{}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "<<" || key.Anchor != "" {
				return nil, fmt.Errorf("plugin.input: mapping keys must be strings")
			}
			if _, ok := out[key.Value]; ok {
				return nil, fmt.Errorf("plugin.input: duplicate key %q", key.Value)
			}
			value, err := pluginJSON(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			out[key.Value] = value
		}
		return out, nil
	case yaml.SequenceNode:
		if n.Tag != "!!seq" {
			return nil, fmt.Errorf("plugin.input: unsupported YAML array tag")
		}
		out := make([]any, 0, len(n.Content))
		for _, child := range n.Content {
			value, err := pluginJSON(child)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		return out, nil
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!str":
			return n.Value, nil
		case "!!null":
			return nil, nil
		case "!!bool":
			var v bool
			if err := n.Decode(&v); err != nil {
				return nil, err
			}
			return v, nil
		case "!!int":
			text := strings.ReplaceAll(n.Value, "_", "")
			v, ok := new(big.Int).SetString(text, 0)
			if !ok {
				v, ok = new(big.Int).SetString(text, 10)
			}
			if !ok {
				return nil, fmt.Errorf("plugin.input: invalid integer")
			}
			return json.Number(v.String()), nil
		case "!!float":
			// YAML resolves integers larger than uint64 as floats; keep their
			// decimal spelling exact rather than passing through float64.
			if integer, ok := new(big.Int).SetString(strings.ReplaceAll(n.Value, "_", ""), 10); ok {
				return json.Number(integer.String()), nil
			}
			var value float64
			if err := n.Decode(&value); err != nil {
				return nil, err
			}
			raw, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			return json.Number(bytes.TrimSpace(raw)), nil
		}
	}
	return nil, fmt.Errorf("plugin.input: expected JSON values")
}
