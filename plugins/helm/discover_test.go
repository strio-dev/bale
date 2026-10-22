package helmplugin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, text := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "test/" + name, Mode: 0600, Size: int64(len(text))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func baseFiles(template string) map[string]string {
	return map[string]string{"Chart.yaml": "apiVersion: v2\nname: test\nversion: 1.0.0\n", "templates/workload.yaml": template}
}
func request(t *testing.T, files map[string]string) Request {
	t.Helper()
	return Request{SchemaVersion: 1, Chart: archive(t, files), Release: "bale-test", Namespace: "default"}
}
func discover(t *testing.T, req Request) Declaration {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	output, err := Discover(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	var declaration Declaration
	if err := json.Unmarshal(output, &declaration); err != nil {
		t.Fatal(err)
	}
	if declaration.SchemaVersion != 1 {
		t.Fatalf("declaration %s", output)
	}
	return declaration
}
func wantRefs(t *testing.T, d Declaration, want ...string) {
	t.Helper()
	got := make([]string, 0, len(d.Dependencies))
	for _, dep := range d.Dependencies {
		got = append(got, dep.Ref)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("dependencies %v, want %v", got, want)
	}
}

const pod = `apiVersion: v1
kind: Pod
metadata:
  name: example
spec:
  containers:
    - name: app
      image: {{ .Values.image | default "busybox" | quote }}
`

func TestValuesOrderAndDefaults(t *testing.T) {
	files := baseFiles(pod)
	files["values.yaml"] = "image: alpine\n"
	req := request(t, files)
	wantRefs(t, discover(t, req), "docker.io/library/alpine:latest")
	req.ValuesFiles = []string{"image: nginx\n---\nimage: redis\n", "image: ubuntu\n"}
	wantRefs(t, discover(t, req), "docker.io/library/ubuntu:latest")
	req.Values = map[string]any{"image": "busybox:1.36"}
	req.Context = "production"
	d := discover(t, req)
	wantRefs(t, d, "docker.io/library/busybox:1.36")
	if d.Context != "production" {
		t.Fatal(d.Context)
	}
	req.ValuesFiles = []string{"image: [invalid"}
	data, _ := json.Marshal(req)
	if _, err := Discover(context.Background(), data); err == nil || !strings.Contains(err.Error(), "valuesFiles[0]") {
		t.Fatalf("error %v", err)
	}
}

func TestWorkloadPathsAndFalsePositives(t *testing.T) {
	cases := []struct{ api, kind, body string }{
		{"v1", "Pod", "spec:\n  containers: [{name: app, image: busybox}]\n"},
		{"v1", "PodTemplate", "template:\n  spec:\n    containers: [{name: app, image: busybox}]\n"},
		{"v1", "ReplicationController", "spec:\n  template:\n    spec:\n      containers: [{name: app, image: busybox}]\n"},
		{"apps/v1", "Deployment", "spec:\n  template:\n    spec:\n      containers: [{name: app, image: busybox}]\n"},
		{"apps/v1", "StatefulSet", "spec:\n  template:\n    spec:\n      containers: [{name: app, image: busybox}]\n"},
		{"apps/v1", "DaemonSet", "spec:\n  template:\n    spec:\n      containers: [{name: app, image: busybox}]\n"},
		{"apps/v1", "ReplicaSet", "spec:\n  template:\n    spec:\n      containers: [{name: app, image: busybox}]\n"},
		{"batch/v1", "Job", "spec:\n  template:\n    spec:\n      containers: [{name: app, image: busybox}]\n"},
		{"batch/v1", "CronJob", "spec:\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers: [{name: app, image: busybox}]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			text := fmt.Sprintf("apiVersion: %s\nkind: %s\n%s", tc.api, tc.kind, tc.body)
			wantRefs(t, discover(t, request(t, baseFiles(text))), "docker.io/library/busybox:latest")
		})
	}
	falsePositive := `apiVersion: example.test/v1
kind: Deployment
spec:
  template:
    spec:
      containers: [{image: must-not-find}]
---
apiVersion: v1
kind: ConfigMap
data:
  image: must-not-find
spec:
  containers: [{image: must-not-find}]
`
	req := request(t, baseFiles(falsePositive))
	req.AdditionalImages = []string{"operator/image:v1"}
	wantRefs(t, discover(t, req), "docker.io/operator/image:v1")
}

func TestListHooksNormalizationAndDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	text := `apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: Pod
  metadata:
    annotations: {helm.sh/hook: pre-install}
  spec:
    containers: [{name: app, image: busybox}, {name: pinned, image: nginx:ignored@DIGEST}]
    initContainers: [{name: init, image: alpine:3}]
    ephemeralContainers: [{name: debug, image: busybox:latest}]
- apiVersion: v1
  kind: List
  items:
  - apiVersion: batch/v1
    kind: Job
    spec:
      template:
        spec:
          containers: [{name: worker, image: ghcr.io/example/worker:v2}]
---
# Empty document.
---
`
	files := baseFiles(strings.ReplaceAll(text, "DIGEST", digest))
	files["templates/NOTES.txt"] = "not: valid: yaml"
	req := request(t, files)
	req.AdditionalImages = []string{"busybox", "nginx@" + digest}
	wantRefs(t, discover(t, req), "docker.io/library/alpine:3", "docker.io/library/busybox:latest", "docker.io/library/nginx@"+digest, "ghcr.io/example/worker:v2")
}

func TestCapabilitiesReleaseAndOfflineLookup(t *testing.T) {
	text := `apiVersion: v1
kind: Pod
spec:
  containers:
  - image: {{ if .Release.IsUpgrade }}upgrade{{ else }}install{{ end }}:{{ if .Capabilities.APIVersions.Has "example.test/v1" }}custom{{ else }}default{{ end }}
{{ if lookup "v1" "ConfigMap" "default" "missing" }}{{ fail "unexpected cluster result" }}{{ end }}
`
	req := request(t, baseFiles(text))
	wantRefs(t, discover(t, req), "docker.io/library/install:default")
	req.IsUpgrade = true
	req.Capabilities.APIVersions = []string{"example.test/v1"}
	wantRefs(t, discover(t, req), "docker.io/library/upgrade:custom")
	// Additions must not mutate stock defaults for later independent renders.
	wantRefs(t, discover(t, request(t, baseFiles(text))), "docker.io/library/install:default")
	files := baseFiles(pod)
	files["Chart.yaml"] += "kubeVersion: '>=1.35.0'\n"
	req = request(t, files)
	req.Capabilities.KubeVersion = "1.36.0"
	discover(t, req)
	req.Capabilities.KubeVersion = "1.30.0"
	data, _ := json.Marshal(req)
	if _, err := Discover(context.Background(), data); err == nil || !strings.Contains(err.Error(), "kubeVersion") {
		t.Fatalf("error %v", err)
	}
}

func dependencyFiles() map[string]string {
	files := baseFiles(pod)
	files["Chart.yaml"] += "dependencies:\n- name: child\n  alias: worker\n  version: '^1.0.0'\n  condition: worker.enabled\n  tags: [workers]\n  import-values:\n  - child: exports.images\n    parent: imported\n"
	files["values.yaml"] = "worker:\n  enabled: true\ntags:\n  workers: true\n"
	files["templates/workload.yaml"] = `apiVersion: v1
kind: Pod
spec:
  containers: [{name: parent, image: '{{ (.Values.imported | default dict).image | default "fallback" }}'}]
`
	files["charts/child/Chart.yaml"] = "apiVersion: v2\nname: child\nversion: 1.2.0\ndependencies:\n- name: nested\n  version: 2.x.x\n"
	files["charts/child/values.yaml"] = "image: child\nexports:\n  images:\n    image: imported\n"
	files["charts/child/templates/pod.yaml"] = pod
	files["charts/child/charts/nested/Chart.yaml"] = "apiVersion: v2\nname: nested\nversion: 2.0.0\n"
	files["charts/child/charts/nested/templates/pod.yaml"] = "apiVersion: v1\nkind: Pod\nspec:\n  containers: [{image: nested}]\n"
	return files
}

func TestDependenciesAliasesConditionsTagsAndImports(t *testing.T) {
	files := dependencyFiles()
	req := request(t, files)
	wantRefs(t, discover(t, req), "docker.io/library/child:latest", "docker.io/library/imported:latest", "docker.io/library/nested:latest")
	req.Values = map[string]any{"worker": map[string]any{"enabled": false}}
	wantRefs(t, discover(t, req), "docker.io/library/fallback:latest")
	delete(files, "values.yaml")
	req = request(t, files)
	req.Values = map[string]any{"tags": map[string]any{"workers": false}}
	wantRefs(t, discover(t, req), "docker.io/library/fallback:latest")
}

func TestMissingAndIncompatibleDependencies(t *testing.T) {
	for _, which := range []string{"missing", "version", "nested"} {
		t.Run(which, func(t *testing.T) {
			files := dependencyFiles()
			switch which {
			case "missing":
				for name := range files {
					if strings.HasPrefix(name, "charts/child/") {
						delete(files, name)
					}
				}
			case "version":
				files["charts/child/Chart.yaml"] = strings.Replace(files["charts/child/Chart.yaml"], "version: 1.2.0", "version: 3.0.0", 1)
			case "nested":
				delete(files, "charts/child/charts/nested/Chart.yaml")
				delete(files, "charts/child/charts/nested/templates/pod.yaml")
			}
			req := request(t, files)
			req.Values = map[string]any{"worker": map[string]any{"enabled": false}}
			data, _ := json.Marshal(req)
			if _, err := Discover(context.Background(), data); err == nil || !strings.Contains(err.Error(), "not bundled") {
				t.Fatalf("error %v", err)
			}
		})
	}
}

func TestFailuresReturnNoDeclaration(t *testing.T) {
	cases := map[string]string{
		"invalid-image":          "apiVersion: v1\nkind: Pod\nspec:\n  containers: [{image: 'INVALID IMAGE'}]\n",
		"missing-containers":     "apiVersion: v1\nkind: Pod\nspec: {}\n",
		"empty-containers":       "apiVersion: v1\nkind: Pod\nspec:\n  containers: []\n",
		"invalid-container-list": "apiVersion: v1\nkind: Pod\nspec:\n  containers: invalid\n",
		"invalid-container":      "apiVersion: v1\nkind: Pod\nspec:\n  containers: [invalid]\n",
		"missing-image":          "apiVersion: v1\nkind: Pod\nspec:\n  containers: [{name: app}]\n",
		"missing-spec":           "apiVersion: apps/v1\nkind: Deployment\nspec: {}\n",
		"list-items":             "apiVersion: v1\nkind: List\nitems: [invalid]\n",
		"invalid-yaml":           "apiVersion: [",
		"template-error":         "{{ fail \"explicit template failure\" }}",
		"required-error":         "{{ required \"required token\" .Values.token }}",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			req := request(t, baseFiles(text))
			data, _ := json.Marshal(req)
			output, err := Discover(context.Background(), data)
			if err == nil || output != nil {
				t.Fatalf("output %s, error %v", output, err)
			}
		})
	}
	files := baseFiles(pod)
	files["values.schema.json"] = `{"type":"object","required":["token"]}`
	req := request(t, files)
	data, _ := json.Marshal(req)
	if output, err := Discover(context.Background(), data); err == nil || output != nil {
		t.Fatalf("output %s, error %v", output, err)
	}
}

func TestStrictRequest(t *testing.T) {
	inputs := []string{
		`[]`, `null`, `{}`, `{"schemaVersion":2}`, `{"schemaVersion":1,"unknown":true}`,
		`{"schemaVersion":1,"schemaVersion":1}`, `{"schemaVersion":1,"values":{"image":"a","image":"b"}}`,
		`{"schemaVersion":1} {}`, `{"schemaVersion":1,"capabilities":{"unknown":true}}`,
		`{"schemaVersion":1,"SchemaVersion":1}`, `{"schemaVersion":1,"chart":[1,2,3]}`, `{"schemaVersion":1,"valuesFiles":[null]}`, `{"schemaVersion":1,"capabilities":{"KubeVersion":"1.36.0"}}`, `{"schemaVersion":1,"capabilities":{"apiVersions":[null]}}`, `{"schemaVersion":1,"values":null}`, `{"schemaVersion":1,"isUpgrade":null}`, `{"schemaVersion":1,"capabilities":null}`, `{"schemaVersion":1,"values":[]}`, `{"schemaVersion":1,"chart":"invalid-base64"}`,
		string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}), strings.Repeat(" ", MaxRequestBytes+1),
	}
	for i, input := range inputs {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			output, err := Discover(context.Background(), []byte(input))
			if err == nil || output != nil {
				t.Fatalf("output %s, error %v", output, err)
			}
		})
	}
	req := request(t, baseFiles(pod))
	req.Release = "INVALID RELEASE"
	data, _ := json.Marshal(req)
	if _, err := Discover(context.Background(), data); err == nil {
		t.Fatal("invalid release accepted")
	}
	req = request(t, baseFiles(pod))
	req.Namespace = " \n"
	data, _ = json.Marshal(req)
	if _, err := Discover(context.Background(), data); err == nil {
		t.Fatal("blank namespace accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Discover(ctx, []byte(`{}`)); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestDeclarationLimit(t *testing.T) {
	req := request(t, baseFiles("# no resources\n"))
	for i := 0; i < 45000; i++ {
		req.AdditionalImages = append(req.AdditionalImages, fmt.Sprintf("example.test/%s%d:v1", strings.Repeat("a", 80), i))
	}
	input, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(input) > MaxRequestBytes {
		t.Fatalf("test request too large: %d", len(input))
	}
	output, err := Discover(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "declaration exceeds") || output != nil {
		t.Fatalf("output bytes %d, error %v", len(output), err)
	}
}

func TestInlineNumbersPreserveExactImageTags(t *testing.T) {
	req := request(t, baseFiles(`apiVersion: v1
kind: Pod
spec:
  containers:
  - name: app
    image: busybox:{{ printf "%v" .Values.tag }}
`))
	// Quote YAML identifiers to avoid stock Helm LoadValues' float64 conversion.
	req.ValuesFiles = []string{"tag: '9007199254740993'\n"}
	wantRefs(t, discover(t, req), "docker.io/library/busybox:9007199254740993")
	req.ValuesFiles = nil
	req.Values = map[string]any{"tag": json.Number("9007199254740993")}
	wantRefs(t, discover(t, req), "docker.io/library/busybox:9007199254740993")
	// Token validation must also accept valid JSON numbers beyond float64 range.
	req.Values = map[string]any{"tag": json.Number("1e400")}
	wantRefs(t, discover(t, req), "docker.io/library/busybox:1e400")
}
