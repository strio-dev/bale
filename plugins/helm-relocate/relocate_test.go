package relocate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	bale "github.com/strio-dev/bale"
	"go.yaml.in/yaml/v3"
)

var chartDigest = digest.FromString("chart")
var imageDigest = digest.FromString("image")
var chart = "target.example/charts/demo@" + chartDigest.String()
var image = "target.example/images/nginx@" + imageDigest.String()

func record() bale.DestinationRecord {
	return bale.DestinationRecord{SchemaVersion: 1, Bundle: "target.example/bundles/demo@" + digest.FromString("bundle").String(), Source: "source.example/charts/demo@" + chartDigest.String(), Destination: chart, Resolutions: []bale.DestinationResolution{{Capture: digest.FromString("capture"), Outcome: "selected", Declaration: &bale.DestinationDeclaration{Digest: digest.FromString("declaration"), Context: "production"}, Dependencies: []bale.DestinationDependency{{Ref: "docker.io/library/nginx:latest", Source: "docker.io/library/nginx@" + imageDigest.String(), Destination: image}}}}}
}
func envelope(t *testing.T, r bale.DestinationRecord, id string) map[string]any {
	t.Helper()
	raw, err := bale.EncodeDestinationRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	var obj any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"repository": "target.example/charts/demo", "subject": v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: chartDigest, Size: 100}, "descriptor": v1.Descriptor{MediaType: v1.MediaTypeImageManifest, ArtifactType: "application/vnd.bale.destination.v1+json", Digest: digest.FromString(id), Size: 100}, "record": obj, "local": true}
}
func encode(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func inventory(t *testing.T) []byte { return encode(t, []any{envelope(t, record(), "record")}) }

const pod = "apiVersion: v1\nkind: Pod\nspec:\n  containers: [{name: app, image: nginx}]\n"

func TestWorkloadPaths(t *testing.T) {
	cases := []struct{ api, kind, path string }{
		{"v1", "Pod", "spec"}, {"v1", "PodTemplate", "template.spec"}, {"v1", "ReplicationController", "spec.template.spec"},
		{"apps/v1", "Deployment", "spec.template.spec"}, {"apps/v1", "StatefulSet", "spec.template.spec"}, {"apps/v1", "DaemonSet", "spec.template.spec"}, {"apps/v1", "ReplicaSet", "spec.template.spec"},
		{"batch/v1", "Job", "spec.template.spec"}, {"batch/v1", "CronJob", "spec.jobTemplate.spec.template.spec"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			spec := map[string]any{}
			for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
				spec[field] = []any{map[string]any{"name": "app", "image": "nginx", "command": []string{"echo", "nginx"}}}
			}
			var value any = spec
			parts := strings.Split(tc.path, ".")
			for i := len(parts) - 1; i >= 0; i-- {
				value = map[string]any{parts[i]: value}
			}
			object := value.(map[string]any)
			object["apiVersion"] = tc.api
			object["kind"] = tc.kind
			object["metadata"] = map[string]any{"annotations": map[string]string{"helm.sh/hook": "pre-install", "helm.sh/post-renderer-filename": "templates/hook.yaml"}}
			raw := encode(t, object)
			out, err := Relocate(raw, inventory(t), Options{Chart: chart})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(out), image) != 3 || !bytes.Contains(out, []byte("templates/hook.yaml")) || !bytes.Contains(out, []byte(`"nginx"`)) {
				t.Fatalf("unexpected output: %s", out)
			}
		})
	}
}
func TestRecursiveListAndIdempotence(t *testing.T) {
	input := "# Source: test/templates/pod.yaml\napiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: List\n  items:\n  - apiVersion: v1\n    kind: Pod\n    spec:\n      containers: [{name: app, image: nginx}]\n"
	out, err := Relocate([]byte(input), inventory(t), Options{Chart: chart})
	if err != nil {
		t.Fatal(err)
	}
	again, err := Relocate(out, inventory(t), Options{Chart: chart})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, again) || !bytes.Contains(out, []byte("# Source: test/templates/pod.yaml")) {
		t.Fatalf("not stable: %s", again)
	}
}
func TestImageNormalization(t *testing.T) {
	for _, ref := range []string{"nginx", "nginx:latest", "index.docker.io/library/nginx:latest", "docker.io/library/nginx@" + imageDigest.String(), "nginx:ignored@" + imageDigest.String(), image} {
		t.Run(ref, func(t *testing.T) {
			input := strings.Replace(pod, "image: nginx", "image: "+ref, 1)
			out, err := Relocate([]byte(input), inventory(t), Options{Chart: chart})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(out, []byte(image)) {
				t.Fatalf("missing destination: %s", out)
			}
		})
	}
}
func TestMixedMappedAndUnmappedImages(t *testing.T) {
	unmappedDigest := "custom.example/app:release@" + digest.FromString("custom").String()
	input := `apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: Pod
  spec:
    containers:
    - {name: mapped-main, image: nginx}
    - {name: short-main, image: busybox}
    - {name: tagged-main, image: index.docker.io/library/busybox:custom}
    initContainers:
    - {name: mapped-init, image: nginx:latest}
    - {name: digest-init, image: ` + unmappedDigest + `}
    ephemeralContainers:
    - {name: mapped-ephemeral, image: nginx}
    - {name: short-ephemeral, image: alpine:edge}
- apiVersion: batch/v1
  kind: Job
  metadata:
    annotations: {helm.sh/hook: post-install}
  spec:
    template:
      spec:
        containers:
        - {name: mapped-hook, image: nginx}
        - {name: custom-hook, image: target.example/manual/app:release}
`
	want := map[string]string{
		"mapped-main": image, "short-main": "busybox",
		"tagged-main": "index.docker.io/library/busybox:custom",
		"mapped-init": image, "digest-init": unmappedDigest,
		"mapped-ephemeral": image, "short-ephemeral": "alpine:edge",
		"mapped-hook": image, "custom-hook": "target.example/manual/app:release",
	}
	out, err := Relocate([]byte(input), inventory(t), Options{Chart: chart})
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	var visit func(*yaml.Node)
	visit = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			f, err := fields(n)
			if err != nil {
				t.Fatal(err)
			}
			if name, img := f["name"], f["image"]; name != nil && img != nil {
				expected, ok := want[name.Value]
				if !ok || img.Value != expected {
					t.Fatalf("image for %s: got %q, want %q", name.Value, img.Value, expected)
				}
				delete(want, name.Value)
			}
		}
		for _, child := range n.Content {
			visit(child)
		}
	}
	visit(&doc)
	if len(want) != 0 {
		t.Fatalf("missing containers: %v", want)
	}
	again, err := Relocate(out, inventory(t), Options{Chart: chart})
	if err != nil || !bytes.Equal(out, again) {
		t.Fatalf("mixed images not idempotent: %s, %v", again, err)
	}
}
func TestUnknownObjectsUnchanged(t *testing.T) {
	input := "# untouched fields\napiVersion: example.org/v1\nkind: Custom\nimage: nginx\nconfig: &config {image: nginx}\ncopy: *config\n---\napiVersion: v1\nkind: ConfigMap\ndata:\n  image: nginx\n"
	out, err := Relocate([]byte(input), inventory(t), Options{Chart: chart})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(image)) || strings.Count(string(out), "nginx") != 3 || !bytes.Contains(out, []byte("*config")) {
		t.Fatalf("changed unknown data: %s", out)
	}
}
func TestEmptyDependenciesAndChart(t *testing.T) {
	r := record()
	r.Resolutions[0].Dependencies = nil
	raw := encode(t, []any{envelope(t, r, "empty")})
	for _, input := range []string{"", "apiVersion: v1\nkind: ConfigMap\ndata: {message: hello}\n"} {
		if _, err := Relocate([]byte(input), raw, Options{Chart: chart}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := Relocate([]byte(pod), raw, Options{Chart: chart})
	if err != nil || !bytes.Contains(out, []byte("image: nginx")) {
		t.Fatalf("empty declaration did not preserve valid image: %s, %v", out, err)
	}
}
func TestRecordAndResolutionSelection(t *testing.T) {
	r := record()
	old := record()
	old.Bundle = "target.example/bundles/old@" + digest.FromString("old").String()
	historical := envelope(t, r, "historical")
	historical["record"].(map[string]any)["destination"] = "old.example/chart@" + chartDigest.String()
	historical["local"] = true
	local := envelope(t, r, "local")
	local["local"] = false
	raw := encode(t, []any{historical, local})
	if _, err := Relocate([]byte(pod), raw, Options{Chart: chart}); err != nil {
		t.Fatalf("local recomputation: %v", err)
	}
	raw = encode(t, []any{local, envelope(t, old, "old")})
	if _, err := Relocate([]byte(pod), raw, Options{Chart: chart}); err == nil {
		t.Fatal("ambiguous records accepted")
	}
	if _, err := Relocate([]byte(pod), raw, Options{Chart: chart, Bundle: r.Bundle, Source: r.Source}); err != nil {
		t.Fatal(err)
	}
	raw = encode(t, []any{local, local})
	if _, err := Relocate([]byte(pod), raw, Options{Chart: chart}); err != nil {
		t.Fatalf("identical entry dedupe: %v", err)
	}
	second := r.Resolutions[0]
	second.Capture = digest.FromString("capture2")
	second.Declaration = &bale.DestinationDeclaration{Digest: digest.FromString("declaration2"), Context: "test"}
	r.Resolutions = append(r.Resolutions, second)
	raw = encode(t, []any{envelope(t, r, "multiple")})
	if _, err := Relocate([]byte(pod), raw, Options{Chart: chart}); err == nil {
		t.Fatal("ambiguous resolutions accepted")
	}
	for _, o := range []Options{{Chart: chart, Capture: second.Capture.String()}, {Chart: chart, Declaration: second.Declaration.Digest.String()}, {Chart: chart, Context: "test"}} {
		if _, err := Relocate([]byte(pod), raw, o); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range []Options{{Chart: chart, Context: "missing"}, {Chart: chart, Source: "wrong.example/chart@" + chartDigest.String()}} {
		if _, err := Relocate([]byte(pod), raw, o); err == nil {
			t.Fatal("unmatched selector accepted")
		}
	}
}
func TestUnavailableResolutions(t *testing.T) {
	for _, outcome := range []string{"absent", "disabled"} {
		t.Run(outcome, func(t *testing.T) {
			r := record()
			r.Resolutions[0].Outcome = outcome
			r.Resolutions[0].Declaration = nil
			r.Resolutions[0].Dependencies = nil
			if outcome == "disabled" {
				r.Resolutions[0].Policy.Follow = "none"
			}
			raw := encode(t, []any{envelope(t, r, outcome)})
			if out, err := Relocate(nil, raw, Options{Chart: chart}); err == nil || out != nil {
				t.Fatal("unavailable resolution accepted")
			}
		})
	}
}
func TestNormalizedConflicts(t *testing.T) {
	r := record()
	d := r.Resolutions[0].Dependencies[0]
	d.Ref = "index.docker.io/library/nginx:latest"
	d.Source = "index.docker.io/library/nginx@" + imageDigest.String()
	d.Destination = "other.example/nginx@" + imageDigest.String()
	r.Resolutions[0].Dependencies = append(r.Resolutions[0].Dependencies, d)
	raw := encode(t, []any{envelope(t, r, "conflict")})
	if out, err := Relocate([]byte(pod), raw, Options{Chart: chart}); err == nil || out != nil {
		t.Fatal("normalization conflict accepted")
	}
	// A destination alias must not be usable as an unrelated source alias.
	r = record()
	d = r.Resolutions[0].Dependencies[0]
	d.Ref = image
	d.Source = image
	d.Destination = "other.example/nginx@" + imageDigest.String()
	r.Resolutions[0].Dependencies = append(r.Resolutions[0].Dependencies, d)
	if out, err := Relocate([]byte(pod), encode(t, []any{envelope(t, r, "collision")}), Options{Chart: chart}); err == nil || out != nil {
		t.Fatal("destination/source collision accepted")
	}
}
func TestStrictInspectionJSON(t *testing.T) {
	good := string(inventory(t))
	cases := []string{"null", "{}", "[] {}", good + " []", strings.Replace(good, `"local":true`, `"LOCAL":true`, 1), strings.Replace(good, `"local":true`, `"local":null`, 1), strings.Replace(good, `"local":true`, `"local":true,"local":false`, 1), strings.Replace(good, `"local":true`, `"local":true,"unknown":1`, 1), strings.Replace(good, `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1), strings.Replace(good, `"schemaVersion":1`, `"schemaVersion":1,"unknown":1`, 1), strings.Replace(good, `"size":100`, `"size":-1`, 1), strings.Replace(good, `application/vnd.bale.destination.v1+json`, `application/vnd.other`, 1), strings.Replace(good, `"repository":"target.example/charts/demo"`, `"repository":"target.example/charts/demo:tag"`, 1), strings.Replace(good, `"local":true,`, "", 1)}
	for i, input := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if out, err := Relocate([]byte(pod), []byte(input), Options{Chart: chart}); err == nil || out != nil {
				t.Fatalf("invalid input accepted: %s", input)
			}
		})
	}
}
func TestMalformedWorkloadsNoOutput(t *testing.T) {
	cases := []string{
		strings.Replace(pod, "image: nginx", "image: '@bad'", 1), strings.Replace(pod, "image: nginx", "image: 42", 1),
		strings.Replace(pod, "image: nginx", "image: nginx, image: nginx", 1),
		strings.Replace(pod, "spec:", "spec: &spec", 1), strings.Replace(pod, "image: nginx", "image: &image nginx", 1),
		"apiVersion: v1\nkind: Pod\nspec: {containers: &containers [{image: nginx}]}\n",
		"apiVersion: v1\nkind: Pod\nspec: {containers: [&container {image: nginx}]}\n",
		"apiVersion: v1\nkind: Pod\nbase: &base {containers: [{image: nginx}]}\nspec: *base\n",
		"apiVersion: v1\nkind: Pod\nbase: &base {containers: [{image: nginx}]}\nspec: {<<: *base}\n",
		"apiVersion: v1\nkind: Pod\nkind: ConfigMap\nspec: {containers: [{image: nginx}]}\n",
		"apiVersion: v1\napiVersion: example/v1\nkind: Unknown\n",
		"apiVersion: v1\nkind: Pod\nspec: {containers: []}\n",
		"apiVersion: v1\nkind: Pod\nspec: {containers: [{image: nginx}], initContainers: false}\n",
		"apiVersion: v1\nkind: List\nitems: [null]\n",
		"base: &base {apiVersion: v1, kind: Pod}\n<<: *base\nspec: {containers: [{image: nginx}]}\n",
		"apiVersion: v1\nkind: Pod\nspec: {containers: [{image: nginx}]}\nspec: {}\n",
		"apiVersion: apps/v1\nkind: Deployment\nspec: {}\n",
	}
	for i, input := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if out, err := Relocate([]byte(pod+"---\n"+input), inventory(t), Options{Chart: chart}); err == nil || out != nil {
				t.Fatalf("invalid workload accepted: %s\n%s", input, out)
			}
		})
	}
}
func TestLimitsAndOptions(t *testing.T) {
	for _, o := range []Options{{}, {Chart: "nginx"}, {Chart: chart, Capture: "bad"}, {Chart: chart, Declaration: "bad"}, {Chart: chart, Context: " space"}, {Chart: chart, Bundle: "tag.example/bundle:tag"}} {
		if _, err := Relocate(nil, inventory(t), o); err == nil {
			t.Fatalf("invalid options accepted: %+v", o)
		}
	}
	if _, err := Relocate(make([]byte, MaxManifestBytes+1), inventory(t), Options{Chart: chart}); err == nil {
		t.Fatal("manifest size limit")
	}
	if _, err := Relocate(nil, make([]byte, MaxRecordsBytes+1), Options{Chart: chart}); err == nil {
		t.Fatal("records size limit")
	}
}
