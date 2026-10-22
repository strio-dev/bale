// Package config parses the portable, destination-free Bale recipe.
package config

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

type Recipe struct {
	Include []Selection `json:"include"`
}
type Selection struct {
	Ref          string            `json:"ref,omitempty"`
	Repository   string            `json:"repository,omitempty"`
	Tags         *[]string         `json:"tags,omitempty"`
	As           string            `json:"as,omitempty"`
	Referrers    Policy            `json:"referrers"`
	Dependencies dependency.Policy `json:"dependencies"`
	Plugin       *Plugin           `json:"plugin,omitempty"`
}
type Policy struct {
	Include string             `json:"include"`
	Require []RequiredReferrer `json:"require"`
}
type RequiredReferrer struct {
	Digest digest.Digest `json:"digest"`
}

// Parse validates YAML nodes before interpreting values, then resolves defaults.
func Parse(data []byte) (Recipe, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return Recipe{}, fmt.Errorf("recipe: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return Recipe{}, fmt.Errorf("recipe: expected exactly one YAML document")
	}
	if err := validateRecipeNodes(&doc); err != nil {
		return Recipe{}, err
	}
	if len(doc.Content) != 1 {
		return Recipe{}, fmt.Errorf("recipe: expected a mapping")
	}
	root, err := yamlutil.Fields(doc.Content[0], "defaults", "include")
	if err != nil {
		return Recipe{}, err
	}
	policy := Policy{Include: "all", Require: []RequiredReferrer{}}
	dependencyPolicy := dependency.Policy{Follow: "auto"}
	if defaults := root["defaults"]; defaults != nil {
		m, err := yamlutil.Fields(defaults, "referrers", "dependencies")
		if err != nil {
			return Recipe{}, err
		}
		if m["dependencies"] != nil {
			dependencyPolicy, err = parseDependencies(m["dependencies"], dependencyPolicy)
			if err != nil {
				return Recipe{}, err
			}
		}
		if m["referrers"] != nil {
			policy, err = parsePolicy(m["referrers"], policy)
			if err != nil {
				return Recipe{}, err
			}
		}
	}
	inc := root["include"]
	if inc == nil || inc.Kind != yaml.SequenceNode {
		return Recipe{}, fmt.Errorf("include: expected an array")
	}
	r := Recipe{Include: make([]Selection, 0, len(inc.Content))}
	for i, n := range inc.Content {
		s := Selection{Referrers: Policy{Include: policy.Include, Require: append([]RequiredReferrer{}, policy.Require...)}, Dependencies: dependencyPolicy}
		if n.Kind == yaml.ScalarNode {
			s.Ref, err = yamlutil.Text(n)
		} else {
			var m map[string]*yaml.Node
			m, err = yamlutil.Fields(n, "ref", "repository", "tags", "as", "referrers", "dependencies", "plugin")
			if err == nil {
				for key, dest := range map[string]*string{"ref": &s.Ref, "repository": &s.Repository, "as": &s.As} {
					if m[key] != nil {
						*dest, err = yamlutil.Text(m[key])
						if err != nil {
							break
						}
					}
				}
				if err == nil && m["tags"] != nil {
					n := m["tags"]
					if n.Kind != yaml.SequenceNode {
						err = fmt.Errorf("tags: expected an array")
					} else {
						tags := make([]string, 0, len(n.Content))
						for _, t := range n.Content {
							var value string
							value, err = yamlutil.Text(t)
							if err != nil {
								break
							}
							tags = append(tags, value)
						}
						s.Tags = &tags
					}
				}
				if err == nil && m["plugin"] != nil {
					s.Plugin, err = parsePlugin(m["plugin"])
				}
				if err == nil && m["dependencies"] != nil {
					s.Dependencies, err = parseDependencies(m["dependencies"], s.Dependencies)
				}
				if err == nil && m["referrers"] != nil {
					s.Referrers, err = parsePolicy(m["referrers"], s.Referrers)
				}
			}
		}
		if err != nil {
			return Recipe{}, fmt.Errorf("include[%d]: %w", i, err)
		}
		r.Include = append(r.Include, s)
	}
	return Normalize(r)
}

func parsePolicy(n *yaml.Node, p Policy) (Policy, error) {
	m, err := yamlutil.Fields(n, "include", "require")
	if err != nil {
		return p, err
	}
	if m["include"] != nil {
		p.Include, err = yamlutil.Text(m["include"])
		if err != nil {
			return p, err
		}
	}
	if n := m["require"]; n != nil {
		if n.Kind != yaml.SequenceNode {
			return p, fmt.Errorf("require: expected an array")
		}
		p.Require = []RequiredReferrer{}
		for _, item := range n.Content {
			m, err := yamlutil.Fields(item, "digest")
			if err != nil {
				return p, err
			}
			if m["digest"] == nil {
				return p, fmt.Errorf("require: digest is required")
			}
			value, err := yamlutil.Text(m["digest"])
			if err != nil {
				return p, err
			}
			d := digest.Digest(value)
			if err := d.Validate(); err != nil {
				return p, fmt.Errorf("require: %w", err)
			}
			p.Require = append(p.Require, RequiredReferrer{Digest: d})
		}
	}
	return p, nil
}

// Normalize applies recipe defaults and returns an independent, validated copy.
// It accepts the same selections as Parse without requiring callers to construct
// canonical referrer policies themselves.
func Normalize(r Recipe) (Recipe, error) {
	result := Recipe{Include: make([]Selection, len(r.Include))}
	for i, selection := range r.Include {
		result.Include[i] = selection
		s := &result.Include[i]
		if selection.Plugin != nil {
			p := *selection.Plugin
			p.Input = append([]byte(nil), p.Input...)
			if len(p.Input) == 0 {
				p.Input = []byte("{}")
			}
			if p.Blobs != nil {
				p.Blobs = make(map[string]PluginBlob, len(selection.Plugin.Blobs))
				for key, blob := range selection.Plugin.Blobs {
					p.Blobs[key] = blob
				}
			}
			s.Plugin = &p
		}
		if selection.Tags != nil {
			tags := append([]string{}, (*selection.Tags)...)
			s.Tags = &tags
		}
		if s.Referrers.Include == "" {
			s.Referrers.Include = "all"
		}
		required := append([]RequiredReferrer{}, selection.Referrers.Require...)
		sort.Slice(required, func(i, j int) bool { return required[i].Digest < required[j].Digest })
		unique := required[:0]
		for _, item := range required {
			if len(unique) == 0 || unique[len(unique)-1].Digest != item.Digest {
				unique = append(unique, item)
			}
		}
		s.Referrers.Require = unique
		var err error
		s.Dependencies, err = dependency.NormalizePolicy(selection.Dependencies)
		if err != nil {
			return Recipe{}, fmt.Errorf("include[%d]: %w", i, err)
		}

	}
	if err := Validate(result); err != nil {
		return Recipe{}, err
	}
	return result, nil
}

// Validate checks the normalized JSON recipe as well as parsed YAML recipes.
func Validate(r Recipe) error {
	if len(r.Include) == 0 {
		return fmt.Errorf("include: at least one input is required")
	}
	aliases := map[string]bool{}
	for i, s := range r.Include {
		if (s.Ref == "") == (s.Repository == "") {
			return fmt.Errorf("include[%d]: exactly one of ref or repository is required", i)
		}
		value := s.Ref
		if s.Repository != "" {
			value = s.Repository
		}
		ref, err := registry.ParseReference(value)
		if err != nil {
			return fmt.Errorf("include[%d]: %w", i, err)
		}
		if ref.Registry != "localhost" && !strings.ContainsAny(ref.Registry, ".:") {
			return fmt.Errorf("include[%d]: explicit registry host is required", i)
		}
		if s.Ref != "" {
			if s.Tags != nil || ref.Reference == "" {
				return fmt.Errorf("include[%d]: ref needs an explicit tag or digest and cannot have tags", i)
			}
			if err := dependency.ValidateReference(s.Ref); err != nil {
				return fmt.Errorf("include[%d]: %w", i, err)
			}
		} else {
			if ref.Reference != "" || s.Tags == nil {
				return fmt.Errorf("include[%d]: repository requires a tags array and no tag/digest", i)
			}
			seen := map[string]bool{}
			for _, tag := range *s.Tags {
				if err := (registry.Reference{Reference: tag}).ValidateReferenceAsTag(); err != nil {
					return err
				}
				if seen[tag] {
					return fmt.Errorf("include[%d]: duplicate tag %q", i, tag)
				}
				seen[tag] = true
			}
		}
		if _, err := dependency.NormalizePolicy(s.Dependencies); err != nil {
			return fmt.Errorf("include[%d]: %w", i, err)
		}
		if s.Plugin != nil {
			if err := validatePlugin(*s.Plugin, s.Dependencies); err != nil {
				return fmt.Errorf("include[%d]: plugin: %w", i, err)
			}
		}
		if s.As != "" {
			if strings.TrimSpace(s.As) != s.As {
				return fmt.Errorf("include[%d]: invalid alias", i)
			}
			if aliases[s.As] {
				return fmt.Errorf("duplicate alias %q", s.As)
			}
			aliases[s.As] = true
		}
		if s.Referrers.Include != "all" && s.Referrers.Include != "none" {
			return fmt.Errorf("include[%d]: referrers.include must be all or none", i)
		}
		if s.Referrers.Require == nil {
			return fmt.Errorf("include[%d]: referrers.require must be an array", i)
		}
		for j, req := range s.Referrers.Require {
			if err := req.Digest.Validate(); err != nil {
				return err
			}
			if j > 0 && s.Referrers.Require[j-1].Digest >= req.Digest {
				return fmt.Errorf("include[%d]: require must be sorted and unique", i)
			}
		}
	}
	return nil
}

func parseDependencies(n *yaml.Node, p dependency.Policy) (dependency.Policy, error) {
	m, err := yamlutil.Fields(n, "follow", "select")
	if err != nil {
		return p, err
	}
	if m["follow"] != nil {
		p.Follow, err = yamlutil.Text(m["follow"])
		if err != nil {
			return p, err
		}
		// An explicit disable clears inherited selection; an explicit select below
		// still fails normalization if it conflicts with disabled following.
		if p.Follow == "none" {
			p.Select = nil
		}
	}
	if m["select"] != nil {
		selector, err := yamlutil.Fields(m["select"], "digest", "context")
		if err != nil {
			return p, err
		}
		p.Select = &dependency.Selector{}
		if selector["digest"] != nil {
			value, err := yamlutil.Text(selector["digest"])
			if err != nil {
				return p, err
			}
			p.Select.Digest = digest.Digest(value)
		}
		if selector["context"] != nil {
			p.Select.Context, err = yamlutil.Text(selector["context"])
			if err != nil {
				return p, err
			}
		}
	}
	return dependency.NormalizePolicy(p)
}
