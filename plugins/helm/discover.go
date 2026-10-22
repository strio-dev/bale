// Package helmplugin discovers OCI image dependencies from an offline Helm render.
package helmplugin

import (
	"bytes"
	"context"
	_ "crypto/sha256" // Register the OCI digest algorithm used by distribution/reference.
	"encoding/json"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/distribution/reference"
	"helm.sh/helm/v4/pkg/chart/common"
	commonutil "helm.sh/helm/v4/pkg/chart/common/util"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
	"helm.sh/helm/v4/pkg/engine"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const MaxRequestBytes = 8 << 20
const MaxDeclarationBytes = 4 << 20

// Request describes one explicit Helm rendering profile. Chart is a packaged tgz
// archive, encoded as base64 by encoding/json; valuesFiles contains YAML text.
type Request struct {
	SchemaVersion    int            `json:"schemaVersion"`
	Chart            []byte         `json:"chart"`
	ValuesFiles      []string       `json:"valuesFiles,omitempty"`
	Values           map[string]any `json:"values,omitempty"`
	Release          string         `json:"release"`
	Namespace        string         `json:"namespace"`
	Capabilities     Capabilities   `json:"capabilities,omitempty"`
	IsUpgrade        bool           `json:"isUpgrade,omitempty"`
	Context          string         `json:"context,omitempty"`
	AdditionalImages []string       `json:"additionalImages,omitempty"`
}

type Capabilities struct {
	KubeVersion string   `json:"kubeVersion,omitempty"`
	APIVersions []string `json:"apiVersions,omitempty"`
}

type Dependency struct {
	Ref string `json:"ref"`
}
type Declaration struct {
	SchemaVersion int          `json:"schemaVersion"`
	Context       string       `json:"context"`
	Dependencies  []Dependency `json:"dependencies"`
}

// Discover returns a bare Bale dependency declaration, or no output on error.
// It performs an offline render: dependencies must be bundled, lookup has no
// cluster client, DNS is disabled, and hooks are inspected without execution.
// This Go API is not a security sandbox; use the WASI command in Bale to deny
// host access, including external schema references handled by stock Helm.
func Discover(ctx context.Context, input []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(input) > MaxRequestBytes {
		return nil, fmt.Errorf("request exceeds %d bytes", MaxRequestBytes)
	}
	if !utf8.Valid(input) {
		return nil, fmt.Errorf("request must be UTF-8")
	}
	if err := uniqueJSON(input); err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	if err := requestFields(input); err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	var req Request
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	if req.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported request schemaVersion %d", req.SchemaVersion)
	}
	if len(req.Chart) == 0 || strings.TrimSpace(req.Namespace) == "" {
		return nil, fmt.Errorf("chart and namespace are required")
	}
	if err := chartutil.ValidateReleaseName(req.Release); err != nil {
		return nil, err
	}
	if req.Context == "" {
		req.Context = "helm-render"
	}
	if strings.TrimSpace(req.Context) != req.Context || strings.TrimSpace(req.Context) == "" {
		return nil, fmt.Errorf("context must be nonblank without surrounding whitespace")
	}
	ch, err := loader.LoadArchive(bytes.NewReader(req.Chart))
	if err != nil {
		return nil, fmt.Errorf("chart: %w", err)
	}
	if err := checkDependencies(ch); err != nil {
		return nil, err
	}
	values := map[string]any{}
	for i, text := range req.ValuesFiles {
		parsed, err := loader.LoadValues(strings.NewReader(text))
		if err != nil {
			return nil, fmt.Errorf("valuesFiles[%d]: %w", i, err)
		}
		values = loader.MergeMaps(values, parsed)
	}
	values = loader.MergeMaps(values, req.Values)
	if err := chartutil.ProcessDependencies(ch, common.Values(values)); err != nil {
		return nil, err
	}
	caps := common.DefaultCapabilities.Copy()
	caps.APIVersions = slices.Clone(caps.APIVersions)
	if req.Capabilities.KubeVersion != "" {
		kv, err := common.ParseKubeVersion(req.Capabilities.KubeVersion)
		if err != nil {
			return nil, err
		}
		caps.KubeVersion = *kv
	}
	caps.APIVersions = append(caps.APIVersions, req.Capabilities.APIVersions...)
	if ch.Metadata.KubeVersion != "" && !chartutil.IsCompatibleRange(ch.Metadata.KubeVersion, caps.KubeVersion.Version) {
		return nil, fmt.Errorf("chart requires kubeVersion %q, rendering with %q", ch.Metadata.KubeVersion, caps.KubeVersion.Version)
	}
	vals, err := commonutil.ToRenderValues(ch, values, common.ReleaseOptions{Name: req.Release, Namespace: req.Namespace, Revision: 1, IsInstall: !req.IsUpgrade, IsUpgrade: req.IsUpgrade}, caps)
	if err != nil {
		return nil, err
	}
	rendered, err := (engine.Engine{}).RenderWithContext(ctx, ch, vals)
	if err != nil {
		return nil, err
	}
	refs := map[string]bool{}
	names := make([]string, 0, len(rendered))
	for name := range rendered {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if path.Base(name) == "NOTES.txt" || strings.TrimSpace(rendered[name]) == "" {
			continue
		}
		if err := collect(rendered[name], refs); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	for i, image := range req.AdditionalImages {
		if err := addImage(image, refs); err != nil {
			return nil, fmt.Errorf("additionalImages[%d]: %w", i, err)
		}
	}
	ordered := make([]string, 0, len(refs))
	for ref := range refs {
		ordered = append(ordered, ref)
	}
	sort.Strings(ordered)
	deps := make([]Dependency, 0, len(ordered))
	for _, ref := range ordered {
		deps = append(deps, Dependency{Ref: ref})
	}
	output, err := json.Marshal(Declaration{SchemaVersion: 1, Context: req.Context, Dependencies: deps})
	if err != nil {
		return nil, err
	}
	if len(output) > MaxDeclarationBytes {
		return nil, fmt.Errorf("declaration exceeds %d bytes", MaxDeclarationBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return output, nil
}

// Check before processing conditions: offline charts must contain every declared
// dependency, including disabled dependencies, aliases and nested subcharts.
func checkDependencies(ch *chart.Chart) error {
	for _, req := range ch.Metadata.Dependencies {
		found := false
		for _, bundled := range ch.Dependencies() {
			if bundled.Name() == req.Name && chartutil.IsCompatibleRange(req.Version, bundled.Metadata.Version) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("chart %q dependency %q version %q is not bundled in charts/", ch.Name(), req.Name, req.Version)
		}
	}
	for _, dep := range ch.Dependencies() {
		if err := checkDependencies(dep); err != nil {
			return err
		}
	}
	return nil
}

func collect(text string, refs map[string]bool) error {
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(text), 4096)
	for index := 1; ; index++ {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("document %d: %w", index, err)
		}
		if len(doc) == 0 {
			continue
		}
		if err := objectImages(doc, refs); err != nil {
			return fmt.Errorf("document %d: %w", index, err)
		}
	}
}

func objectImages(object map[string]any, refs map[string]bool) error {
	api, _ := object["apiVersion"].(string)
	kind, _ := object["kind"].(string)
	if api == "v1" && kind == "List" {
		items, ok := object["items"].([]any)
		if !ok {
			return fmt.Errorf("list.items must be an array")
		}
		for i, item := range items {
			child, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("list.items[%d] must be an object", i)
			}
			if err := objectImages(child, refs); err != nil {
				return fmt.Errorf("list.items[%d]: %w", i, err)
			}
		}
		return nil
	}
	var fields []string
	switch {
	case api == "v1" && kind == "Pod":
		fields = []string{"spec"}
	case api == "v1" && kind == "PodTemplate":
		fields = []string{"template", "spec"}
	case api == "v1" && kind == "ReplicationController":
		fields = []string{"spec", "template", "spec"}
	case api == "apps/v1" && slices.Contains([]string{"Deployment", "StatefulSet", "DaemonSet", "ReplicaSet"}, kind):
		fields = []string{"spec", "template", "spec"}
	case api == "batch/v1" && kind == "Job":
		fields = []string{"spec", "template", "spec"}
	case api == "batch/v1" && kind == "CronJob":
		fields = []string{"spec", "jobTemplate", "spec", "template", "spec"}
	default:
		return nil
	}
	spec := object
	for _, field := range fields {
		next, ok := spec[field].(map[string]any)
		if !ok {
			return fmt.Errorf("%s %s: %s must be an object", api, kind, strings.Join(fields, "."))
		}
		spec = next
	}
	for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
		raw, exists := spec[field]
		if !exists {
			if field == "containers" {
				return fmt.Errorf("%s %s: containers must be a nonempty array", api, kind)
			}
			continue
		}
		containers, ok := raw.([]any)
		if !ok || (field == "containers" && len(containers) == 0) {
			return fmt.Errorf("%s %s: %s must be an array (containers must be nonempty)", api, kind, field)
		}
		for i, raw := range containers {
			container, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("%s %s: %s[%d] must be an object", api, kind, field, i)
			}
			image, ok := container["image"].(string)
			if !ok {
				return fmt.Errorf("%s %s: %s[%d].image must be a string", api, kind, field, i)
			}
			if err := addImage(image, refs); err != nil {
				return fmt.Errorf("%s %s: %s[%d]: %w", api, kind, field, i, err)
			}
		}
	}
	return nil
}

func addImage(image string, refs map[string]bool) error {
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("image must be nonempty")
	}
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return fmt.Errorf("image %q: %w", image, err)
	}
	if pinned, ok := named.(reference.Digested); ok {
		named, err = reference.WithDigest(reference.TrimNamed(named), pinned.Digest())
		if err != nil {
			return fmt.Errorf("image %q: %w", image, err)
		}
	} else {
		named = reference.TagNameOnly(named)
	}
	refs[named.String()] = true
	return nil
}

// Reject duplicate keys at every nesting level, including values objects. JSON's
// default last-key-wins handling would otherwise hide mistaken rendering inputs.
func uniqueJSON(input []byte) error {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	first, err := dec.Token()
	if err != nil {
		return err
	}
	if first != json.Delim('{') {
		return fmt.Errorf("expected an object")
	}
	if err := walkObject(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}
func walkObject(dec *json.Decoder) error {
	seen := map[string]bool{}
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("expected an object key")
		}
		if seen[key] {
			return fmt.Errorf("duplicate key %q", key)
		}
		seen[key] = true
		if err := walkValue(dec); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}
func walkValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		return walkObject(dec)
	case json.Delim('['):
		for dec.More() {
			if err := walkValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
	}
	return err
}

// encoding/json accepts case-insensitive struct keys and byte arrays for []byte.
// Keep the wire schema exact rather than inheriting those conveniences.
func requestFields(input []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil {
		return err
	}
	for key, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("%s must not be null", key)
		}
		switch key {
		case "schemaVersion", "release", "namespace", "isUpgrade", "context", "values":
		case "chart":
			if len(value) == 0 || value[0] != '"' {
				return fmt.Errorf("chart must be a base64 string")
			}
		case "valuesFiles", "additionalImages":
			if err := stringArray(value); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		case "capabilities":
			var caps map[string]json.RawMessage
			if err := json.Unmarshal(value, &caps); err != nil {
				return fmt.Errorf("capabilities: %w", err)
			}
			for name, raw := range caps {
				if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
					return fmt.Errorf("capabilities.%s must not be null", name)
				}
				switch name {
				case "kubeVersion":
				case "apiVersions":
					if err := stringArray(raw); err != nil {
						return fmt.Errorf("capabilities.apiVersions: %w", err)
					}
				default:
					return fmt.Errorf("unknown capabilities field %q", name)
				}
			}
		default:
			return fmt.Errorf("unknown field %q", key)
		}
	}
	return nil
}
func stringArray(raw []byte) error {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	for i, value := range values {
		if len(value) == 0 || value[0] != '"' {
			return fmt.Errorf("entry %d must be a string", i)
		}
	}
	return nil
}
