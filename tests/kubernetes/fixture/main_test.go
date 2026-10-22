package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"go.yaml.in/yaml/v3"
)

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name, command, source, dest, dir, workload, chart, version, plugin string
		args                                                               []string
		want                                                               string
	}{
		{name: "plugin seed", command: "plugin-seed", source: "registry:5001", dir: "state", plugin: "bale-0.0.0.tgz"},
		{name: "missing plugin archive", command: "plugin-seed", source: "registry:5001", dir: "state", want: "--plugin-archive"},
		{name: "missing plugin source", command: "plugin-seed", dir: "state", plugin: "bale-0.0.0.tgz", want: "--source"},
		{name: "seed", command: "seed", source: "registry:5001", dest: "registry:5002", dir: "state", workload: "app"},
		{name: "retag A", command: "retag", source: "registry:5001", dir: "state", version: "A"},
		{name: "retag B", command: "retag", source: "registry:5001", dir: "state", version: "B"},
		{name: "service seed", command: "service-seed", source: "registry:5001", dir: "state", chart: "chart.tgz"},
		{name: "removed deliver", command: "deliver", dir: "state", want: "unknown subcommand"},
		{name: "removed service deliver", command: "service-deliver", dir: "state", want: "unknown subcommand"},
		{name: "missing directory", command: "seed", want: "--dir"},
		{name: "missing workload", command: "seed", source: "registry:5001", dest: "registry:5002", dir: "state", want: "--workload"},
		{name: "missing service chart", command: "service-seed", source: "registry:5001", dir: "state", want: "--service-chart"},
		{name: "missing retag version", command: "retag", source: "registry:5001", dir: "state", want: "--version"},
		{name: "invalid retag version", command: "retag", source: "registry:5001", dir: "state", version: "C", want: "--version"},
		{name: "unknown command", command: "unknown", dir: "state", want: "unknown subcommand"},
		{name: "registry URL", command: "retag", source: "http://registry:5001", dir: "state", version: "A", want: "host[:port]"},
		{name: "extra argument", command: "deliver", dir: "state", args: []string{"extra"}, want: "positional"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(tc.command, tc.source, tc.dest, tc.dir, tc.workload, tc.chart, tc.version, tc.plugin, tc.args)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v; want %q", err, tc.want)
			}
		})
	}
}

func TestChartRetainsDistinctImageReferences(t *testing.T) {
	const image = "registry:5001/images/app:stable"
	raw := chart(image)
	if !bytes.Equal(raw, chart(image)) {
		t.Fatal("chart archives must be deterministic")
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name] = string(body)
	}
	for name, body := range files {
		if strings.Contains(body, "images/override") || strings.Contains(body, "CUSTOM") {
			t.Fatalf("manual image leaked into chart %s: %s", name, body)
		}
	}
	for _, expected := range []string{"image: " + image, "helper: registry:5001/images/app:b", "imagePullSecrets: []", "imagePullPolicy: IfNotPresent"} {
		if !strings.Contains(files["app/values.yaml"], expected) {
			t.Fatalf("missing %q from values", expected)
		}
	}
	deployment := files["app/templates/deployment.yaml"]
	for _, expected := range []string{"image: {{ .Values.image | quote }}", "image: {{ .Values.helper | quote }}", "initContainers:", "imagePullSecrets:\n{{ toYaml .Values.imagePullSecrets | indent 8 }}"} {
		if !strings.Contains(deployment, expected) {
			t.Fatalf("missing %q from deployment", expected)
		}
	}
	if strings.Count(deployment, "imagePullPolicy: {{ .Values.imagePullPolicy }}") != 2 {
		t.Fatal("main and init containers must use the configured pull policy")
	}
	var parent struct {
		Dependencies []struct {
			Name       string
			Version    string
			Repository string
		}
	}
	if err := yaml.Unmarshal([]byte(files["app/Chart.yaml"]), &parent); err != nil {
		t.Fatal(err)
	}
	if len(parent.Dependencies) != 1 || parent.Dependencies[0].Name != "child" || parent.Dependencies[0].Version != "1.0.0" || parent.Dependencies[0].Repository != "file://charts/child" {
		t.Fatalf("bundled child declaration = %+v", parent.Dependencies)
	}
	var child struct {
		Name    string
		Version string
	}
	if err := yaml.Unmarshal([]byte(files["app/charts/child/Chart.yaml"]), &child); err != nil {
		t.Fatal(err)
	}
	if child.Name != "child" || child.Version != "1.0.0" {
		t.Fatalf("bundled child metadata = %+v", child)
	}
	if _, found := files["app/templates/hook.yaml"]; found {
		t.Fatal("hook must be rendered from the bundled subchart")
	}
	for _, expected := range []string{"image: registry:5001/images/app:hook", "imagePullSecrets: []", "imagePullPolicy: IfNotPresent"} {
		if !strings.Contains(files["app/charts/child/values.yaml"], expected) {
			t.Fatalf("child values missing %q", expected)
		}
	}
	hook := files["app/charts/child/templates/hook.yaml"]
	for _, expected := range []string{"imagePullSecrets:\n{{ toYaml .Values.imagePullSecrets | indent 8 }}", "imagePullPolicy: {{ .Values.imagePullPolicy }}"} {
		if !strings.Contains(hook, expected) {
			t.Fatalf("hook missing %q", expected)
		}
	}
	if !strings.Contains(hook, "post-install,post-upgrade,post-rollback") || !strings.Contains(hook, "image: {{ .Values.image | quote }}") {
		t.Fatal("hook must exercise the main image at every release transition")
	}
}

func TestSeedMetadataContainsOnlyPublishedSources(t *testing.T) {
	raw, err := json.Marshal(fixture{Override: manualImage{Ref: "source:5001/images/override:custom", Digest: "sha256:custom", Version: "CUSTOM"}, Source: "source:5001", Dest: "dest:5002", Chart: "source:5001/charts/app@sha256:chart", Versions: map[string]version{"A": {OriginalImage: "source:5001/images/app:stable", ImageDigest: "sha256:a"}, "B": {OriginalImage: "source:5001/images/app:stable", ImageDigest: "sha256:b"}}})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 5 {
		t.Fatalf("unexpected source metadata fields: %s", raw)
	}
	var override map[string]string
	if err := json.Unmarshal(fields["override"], &override); err != nil {
		t.Fatal(err)
	}
	if len(override) != 3 || override["ref"] != "source:5001/images/override:custom" || override["digest"] != "sha256:custom" || override["version"] != "CUSTOM" {
		t.Fatalf("unexpected manual override: %+v", override)
	}
	var versions map[string]map[string]string
	if err := json.Unmarshal(fields["versions"], &versions); err != nil {
		t.Fatal(err)
	}
	for name, item := range versions {
		if len(item) != 2 || item["original_image"] != "source:5001/images/app:stable" || item["image_digest"] == "" {
			t.Fatalf("%s contains capture/delivery metadata or lacks source metadata: %+v", name, item)
		}
	}
}

func TestTarBytesStableOrderAndMode(t *testing.T) {
	raw := tarBytes(map[string][]byte{"z": []byte("last"), "a": []byte("first")}, 0755)
	tr := tar.NewReader(bytes.NewReader(raw))
	for _, name := range []string{"a", "z"} {
		h, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		if h.Name != name || h.Mode != 0755 {
			t.Fatalf("header = %+v", h)
		}
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatalf("trailing entry: %v", err)
	}
}

func TestHelmChartConfigUsesPackagedMetadata(t *testing.T) {
	archive := func(metadata string) []byte {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		if _, err := gz.Write(tarBytes(map[string][]byte{"bale-flux/Chart.yaml": []byte(metadata), "bale-flux/charts/child/Chart.yaml": []byte("name: ignored\nversion: 9.9.9\n")}, 0644)); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	raw := archive("apiVersion: v2\nname: bale-flux\nversion: 1.2.3+release\nappVersion: 1.2.3\ndescription: Actual packaged chart\n")
	before := bytes.Clone(raw)
	config, version, err := helmChartConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if version != "1.2.3+release" {
		t.Fatalf("version = %q", version)
	}
	for _, field := range []string{`"name":"bale-flux"`, `"version":"1.2.3+release"`, `"description":"Actual packaged chart"`} {
		if !bytes.Contains(config, []byte(field)) {
			t.Fatalf("config missing %s: %s", field, config)
		}
	}
	if !bytes.Equal(raw, before) {
		t.Fatal("chart archive changed")
	}
	for _, metadata := range []string{"apiVersion: v2\nname: bale-flux\n", "apiVersion: v2\nname: bale-flux\nversion: 123\n", "invalid: ["} {
		if _, _, err := helmChartConfig(archive(metadata)); err == nil {
			t.Fatalf("accepted invalid metadata %q", metadata)
		}
	}
	if _, _, err := helmChartConfig([]byte("not gzip")); err == nil {
		t.Fatal("accepted invalid archive")
	}
}

func TestRegistryOptions(t *testing.T) {
	options, err := registryOptions("source:5001", "destination:5002", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(options.PlainHTTPRegistries, []string{"source:5001", "destination:5002"}) || options.Credentials != nil || options.HTTPClient != nil {
		t.Fatalf("default HTTP behavior changed: %+v", options)
	}
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid-ca.pem")
	if err := os.WriteFile(invalid, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, ca := range []string{invalid, filepath.Join(dir, "missing-ca.pem")} {
		if _, err := registryOptions("source:5001", "destination:5002", ca, ""); err == nil {
			t.Fatalf("accepted CA %s", ca)
		}
	}
}

func TestRegistryClientVerifiesTLSAndScopesCredentials(t *testing.T) {
	const username, password = "acceptance", "private-password"
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`)
	want := digest.FromBytes(manifest)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != username || p != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="acceptance"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v2/app/manifests/stable" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", v1.MediaTypeImageManifest)
		w.Header().Set("Docker-Content-Digest", want.String())
		w.Header().Set("Content-Length", fmt.Sprint(len(manifest)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(manifest)
		}
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "https://")
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config.json")
	raw, err := json.Marshal(map[string]any{"auths": map[string]any{host: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(username + ":" + password))}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	options, err := registryOptions("source:5001", host, ca, config)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(options.PlainHTTPRegistries, []string{"source:5001"}) {
		t.Fatalf("plain HTTP hosts: %v", options.PlainHTTPRegistries)
	}
	credential, err := options.Credentials(context.Background(), host)
	if err != nil || credential.Username != username || credential.Password != password {
		t.Fatalf("registry credential: %+v, %v", credential, err)
	}
	other, err := options.Credentials(context.Background(), "other.example:5002")
	if err != nil || other.Username != "" || other.Password != "" || other.AccessToken != "" || other.RefreshToken != "" {
		t.Fatalf("credentials crossed registry host: %+v, %v", other, err)
	}
	client, err := bale.NewClient(options)
	if err != nil {
		t.Fatal(err)
	}
	root, err := client.Resolve(context.Background(), host+"/app:stable")
	if err != nil {
		t.Fatal(err)
	}
	if root.Descriptor.Digest != want {
		t.Fatalf("resolved %s, want %s", root.Descriptor.Digest, want)
	}
	// Removing the trusted CA must fail, even when correct credentials are present.
	options, err = registryOptions("source:5001", "", "", config)
	if err != nil {
		t.Fatal(err)
	}
	client, err = bale.NewClient(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resolve(context.Background(), host+"/app:stable"); err == nil {
		t.Fatal("accepted untrusted registry certificate")
	}
	// Trusting TLS does not bypass registry authentication.
	options, err = registryOptions("source:5001", host, ca, "")
	if err != nil {
		t.Fatal(err)
	}
	client, err = bale.NewClient(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resolve(context.Background(), host+"/app:stable"); err == nil {
		t.Fatal("accepted registry access without credentials")
	}
}
