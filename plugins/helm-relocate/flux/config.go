// Package flux exposes Bale destination selections as Flux Operator ExternalService inputs.
package flux

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/distribution/reference"
	digest "github.com/opencontainers/go-digest"
	relocate "github.com/strio-dev/bale/plugins/helm-relocate"
	"go.yaml.in/yaml/v3"
)

const MaxConfigBytes = 1 << 20

type Config struct {
	Releases []Release `yaml:"releases"`
}
type Release struct {
	Name      string    `yaml:"name"`
	Chart     string    `yaml:"chart"`
	Selection Selectors `yaml:"selection,omitempty"`
}
type Selectors struct {
	Bundle      string `yaml:"bundle,omitempty"`
	Source      string `yaml:"source,omitempty"`
	Capture     string `yaml:"capture,omitempty"`
	Declaration string `yaml:"declaration,omitempty"`
	Context     string `yaml:"context,omitempty"`
}

func (s Selectors) options() relocate.Options {
	return relocate.Options{Bundle: s.Bundle, Source: s.Source, Capture: s.Capture, Declaration: s.Declaration, Context: s.Context}
}

var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// LoadConfig rejects unknown fields, duplicates, nulls, aliases, and multiple documents.
func LoadConfig(r io.Reader) (Config, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxConfigBytes+1))
	if err != nil {
		return Config{}, err
	}
	if len(raw) > MaxConfigBytes {
		return Config{}, fmt.Errorf("configuration exceeds limit")
	}
	var node yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	if err = dec.Decode(&node); err != nil {
		return Config{}, err
	}
	if err = validateNode(&node); err != nil {
		return Config{}, err
	}
	var extra yaml.Node
	if err = dec.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("configuration must contain one document")
	}
	var c Config
	strict := yaml.NewDecoder(bytes.NewReader(raw))
	strict.KnownFields(true)
	if err = strict.Decode(&c); err != nil {
		return Config{}, err
	}
	if err = c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}
func validateNode(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" || n.Tag == "!!null" {
		return fmt.Errorf("null, anchors and aliases are not allowed")
	}
	if n.Kind == yaml.ScalarNode && (n.Tag != "!!str" || strings.TrimSpace(n.Value) == "" || strings.TrimSpace(n.Value) != n.Value) {
		return fmt.Errorf("configuration values must be nonempty strings without surrounding whitespace")
	}
	if n.Kind == yaml.MappingNode {
		if len(n.Content) == 0 {
			return fmt.Errorf("empty configuration objects are not allowed")
		}
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || k.Value == "<<" || seen[k.Value] {
				return fmt.Errorf("duplicate or invalid configuration key %q", k.Value)
			}
			seen[k.Value] = true
		}
	}
	for _, child := range n.Content {
		if err := validateNode(child); err != nil {
			return err
		}
	}
	return nil
}
func (c Config) Validate() error {
	if len(c.Releases) == 0 || len(c.Releases) > 256 {
		return fmt.Errorf("configuration requires between 1 and 256 releases")
	}
	seen := map[string]bool{}
	for _, r := range c.Releases {
		if len(r.Name) > 63 || !namePattern.MatchString(r.Name) || seen[r.Name] {
			return fmt.Errorf("invalid or duplicate release name %q", r.Name)
		}
		seen[r.Name] = true
		n, err := reference.ParseNormalizedNamed(strings.TrimPrefix(r.Chart, "oci://"))
		if err != nil {
			return fmt.Errorf("release %s: invalid chart", r.Name)
		}
		if _, ok := n.(reference.Tagged); !ok {
			if _, ok := n.(reference.Digested); !ok {
				return fmt.Errorf("release %s: chart requires tag or digest", r.Name)
			}
		}
		for _, selector := range []string{r.Selection.Bundle, r.Selection.Source} {
			if selector == "" {
				continue
			}
			n, err := reference.ParseNormalizedNamed(selector)
			if err != nil {
				return fmt.Errorf("release %s: invalid pinned selector", r.Name)
			}
			if _, ok := n.(reference.Digested); !ok {
				return fmt.Errorf("release %s: selector requires digest", r.Name)
			}
			if _, ok := n.(reference.Tagged); ok || n.String() != selector {
				return fmt.Errorf("release %s: selector must be fully qualified without tag", r.Name)
			}
		}
		for _, selector := range []string{r.Selection.Capture, r.Selection.Declaration} {
			if selector != "" {
				if err := digest.Digest(selector).Validate(); err != nil {
					return fmt.Errorf("release %s: invalid digest selector", r.Name)
				}
			}
		}
		if strings.TrimSpace(r.Selection.Context) != r.Selection.Context {
			return fmt.Errorf("release %s: invalid context", r.Name)
		}
	}
	return nil
}
