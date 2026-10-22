// Package relocate applies confirmed Bale destination mappings to Helm-rendered workloads.
package relocate

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/distribution/reference"
	digest "github.com/opencontainers/go-digest"
	"go.yaml.in/yaml/v3"
)

const (
	// MaxRecordsBytes bounds destination inspection data passed through plugin arguments.
	MaxRecordsBytes = 64 << 10
	// MaxManifestBytes bounds the rendered input stream.
	MaxManifestBytes = 8 << 20
	// MaxOutputBytes bounds buffered rewritten output.
	MaxOutputBytes = 16 << 20
)

// Options selects one exact chart, destination record, and dependency resolution.
type Options struct{ Chart, Bundle, Source, Capture, Declaration, Context string }

func pinned(value string) error {
	n, err := reference.ParseNormalizedNamed(value)
	if err != nil {
		return err
	}
	if _, ok := n.(reference.Digested); !ok {
		return fmt.Errorf("must be digest pinned")
	}
	if _, ok := n.(reference.Tagged); ok {
		return fmt.Errorf("tag is not allowed on pinned selector")
	}
	if n.String() != value {
		return fmt.Errorf("must be a fully qualified repository@digest")
	}
	return nil
}
func validateOptions(o Options) error {
	if o.Chart == "" {
		return fmt.Errorf("--chart is required")
	}
	return validateSelectors(o)
}

func validateSelectors(o Options) error {
	for _, v := range []struct{ name, value string }{{"chart", o.Chart}, {"bundle", o.Bundle}, {"source", o.Source}} {
		if v.value != "" {
			if err := pinned(v.value); err != nil {
				return fmt.Errorf("%s: %w", v.name, err)
			}
		}
	}
	for _, v := range []string{o.Capture, o.Declaration} {
		if v != "" {
			if err := digest.Digest(v).Validate(); err != nil {
				return err
			}
		}
	}
	if strings.TrimSpace(o.Context) != o.Context {
		return fmt.Errorf("context has surrounding whitespace")
	}
	return nil
}

func normalize(image string) (string, error) {
	n, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", fmt.Errorf("invalid image %q: %w", image, err)
	}
	if p, ok := n.(reference.Digested); ok {
		n, err = reference.WithDigest(reference.TrimNamed(n), p.Digest())
		if err != nil {
			return "", err
		}
	} else {
		n = reference.TagNameOnly(n)
	}
	return n.String(), nil
}

// Relocate validates the complete stream before returning any rendered output.
func Relocate(manifests, recordsJSON []byte, o Options) ([]byte, error) {
	if len(manifests) > MaxManifestBytes {
		return nil, fmt.Errorf("manifests exceed %d bytes", MaxManifestBytes)
	}
	if err := validateOptions(o); err != nil {
		return nil, err
	}
	selection, err := selectJSON(recordsJSON, o)
	if err != nil {
		return nil, err
	}
	return RelocateSelection(manifests, selection)
}

// RelocateSelection applies a validated selection without another registry lookup.
// Valid image references absent from the selection retain their original spelling.
func RelocateSelection(manifests []byte, selection Selection) ([]byte, error) {
	if len(manifests) > MaxManifestBytes {
		return nil, fmt.Errorf("manifests exceed %d bytes", MaxManifestBytes)
	}
	if selection.inspection.Repository == "" {
		return nil, fmt.Errorf("uninitialized selection")
	}
	mapping := selection.mapping
	dec := yaml.NewDecoder(bytes.NewReader(manifests))
	var out bytes.Buffer
	enc := yaml.NewEncoder(outputWriter{&out})
	enc.SetIndent(2)
	for index := 1; ; index++ {
		var doc yaml.Node
		if err := dec.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("document %d: %w", index, err)
		}
		if len(doc.Content) == 0 {
			continue
		}
		root := doc.Content[0]
		if root.Tag == "!!null" && root.Value == "" {
			continue
		}
		if err := rewrite(root, mapping); err != nil {
			return nil, fmt.Errorf("document %d: %w", index, err)
		}
		if err := enc.Encode(&doc); err != nil {
			return nil, err
		}
	}
	if out.Len() == 0 {
		return []byte{}, nil
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type outputWriter struct{ buffer *bytes.Buffer }

func (w outputWriter) Write(p []byte) (int, error) {
	if len(p) > MaxOutputBytes-w.buffer.Len() {
		return 0, fmt.Errorf("output exceeds %d bytes", MaxOutputBytes)
	}
	return w.buffer.Write(p)
}

// fields validates only the mappings traversed by the recognized workload contract.
func fields(n *yaml.Node) (map[string]*yaml.Node, error) {
	if n.Kind != yaml.MappingNode || n.Anchor != "" {
		return nil, fmt.Errorf("workload path must be an unanchored object")
	}
	m := map[string]*yaml.Node{}
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || k.Value == "<<" {
			return nil, fmt.Errorf("merge or non-string mapping key on workload path")
		}
		if _, ok := m[k.Value]; ok {
			return nil, fmt.Errorf("duplicate workload key %q", k.Value)
		}
		m[k.Value] = v
	}
	return m, nil
}
func discriminators(n *yaml.Node) (string, string, error) {
	if n.Kind != yaml.MappingNode {
		return "", "", fmt.Errorf("manifest must be an object")
	}
	values := map[string]string{}
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Tag == "!!merge" || k.Value == "<<" {
			return "", "", fmt.Errorf("merge key cannot define workload discriminators")
		}
		if k.Value != "apiVersion" && k.Value != "kind" {
			continue
		}
		if _, ok := values[k.Value]; ok {
			return "", "", fmt.Errorf("duplicate %s", k.Value)
		}
		if v.Kind != yaml.ScalarNode || v.Tag != "!!str" || v.Anchor != "" {
			return "", "", fmt.Errorf("%s must be an unanchored string", k.Value)
		}
		values[k.Value] = v.Value
	}
	return values["apiVersion"], values["kind"], nil
}
func rewrite(n *yaml.Node, mapping map[string]string) error {
	api, kind, err := discriminators(n)
	if err != nil {
		return err
	}
	var path []string
	switch {
	case api == "v1" && kind == "List":
		f, err := fields(n)
		if err != nil {
			return err
		}
		items := f["items"]
		if items == nil || items.Kind != yaml.SequenceNode || items.Anchor != "" {
			return fmt.Errorf("list.items must be an unanchored array")
		}
		for _, item := range items.Content {
			if err := rewrite(item, mapping); err != nil {
				return err
			}
		}
		return nil
	case api == "v1" && kind == "Pod":
		path = []string{"spec"}
	case api == "v1" && kind == "PodTemplate":
		path = []string{"template", "spec"}
	case api == "v1" && kind == "ReplicationController":
		path = []string{"spec", "template", "spec"}
	case api == "apps/v1" && (kind == "Deployment" || kind == "StatefulSet" || kind == "DaemonSet" || kind == "ReplicaSet"):
		path = []string{"spec", "template", "spec"}
	case api == "batch/v1" && kind == "Job":
		path = []string{"spec", "template", "spec"}
	case api == "batch/v1" && kind == "CronJob":
		path = []string{"spec", "jobTemplate", "spec", "template", "spec"}
	default:
		return nil
	}
	current := n
	for _, name := range path {
		f, err := fields(current)
		if err != nil {
			return err
		}
		current = f[name]
		if current == nil {
			return fmt.Errorf("%s requires %s", kind, strings.Join(path, "."))
		}
	}
	spec, err := fields(current)
	if err != nil {
		return err
	}
	for _, name := range []string{"containers", "initContainers", "ephemeralContainers"} {
		list := spec[name]
		if list == nil {
			if name == "containers" {
				return fmt.Errorf("containers must be nonempty")
			}
			continue
		}
		if list.Kind != yaml.SequenceNode || list.Anchor != "" || name == "containers" && len(list.Content) == 0 {
			return fmt.Errorf("%s must be an unanchored array (containers nonempty)", name)
		}
		for _, container := range list.Content {
			f, err := fields(container)
			if err != nil {
				return err
			}
			image := f["image"]
			if image == nil || image.Kind != yaml.ScalarNode || image.Tag != "!!str" || image.Anchor != "" {
				return fmt.Errorf("container.image must be an unanchored string")
			}
			key, err := normalize(image.Value)
			if err != nil {
				return err
			}
			if destination, ok := mapping[key]; ok {
				image.Value = destination
			}
		}
	}
	return nil
}
