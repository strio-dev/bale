package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
)

func TestDependenciesHelpAndFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		text string
	}{
		{[]string{"dependencies", "--help"}, "bale dependencies [command]"},
		{[]string{"help", "dependencies"}, "bale dependencies [command]"},
		{[]string{"dependencies", "attach", "--help"}, "bale dependencies attach [options] SUBJECT"},
		{[]string{"help", "dependencies", "attach"}, "bale dependencies attach [options] SUBJECT"},
		{[]string{"dependencies", "list", "--help"}, "bale dependencies list [options] SUBJECT"},
		{[]string{"help", "dependencies", "list"}, "bale dependencies list [options] SUBJECT"},
		{[]string{"dependencies", "ls", "--help"}, "bale dependencies list [options] SUBJECT"},
		{[]string{"help", "dependencies", "ls"}, "bale dependencies list [options] SUBJECT"},
		{[]string{"dependencies", "inspect", "--help"}, "bale dependencies inspect [options] SUBJECT"},
		{[]string{"help", "dependencies", "inspect"}, "bale dependencies inspect [options] SUBJECT"},
	} {
		var out, errs bytes.Buffer
		if code := run(tc.args, &out, &errs); code != 0 || errs.Len() != 0 || !strings.Contains(out.String(), tc.text) {
			t.Fatalf("%v: exit %d stdout %s stderr %s", tc.args, code, &out, &errs)
		}
		if code := run(tc.args, brokenWriter{}, &errs); code != 125 {
			t.Fatalf("output failure exit %d", code)
		}
	}
	for _, args := range [][]string{
		{"dependencies", "attach"}, {"dependencies", "list"}, {"dependencies", "inspect"}, {"dependencies", "other"},
		{"dependencies", "inspect", "example.com/a:v1", "-fjson", "--format=json"},
		{"dependencies", "inspect", "-q"}, {"dependencies", "inspect", "--layout"}, {"dependencies", "inspect", "--select", "production"},
		{"dependencies", "attach", "example.com/a:v1", "-f", "one", "--file", "two"},
		{"dependencies", "list", "example.com/a:v1", "-fjson", "--format=json"},
		{"dependencies", "attach", "-q", "--quiet"},
		{"dependencies", "list", "-q"}, {"dependencies", "attach", "--file"},
		{"dependencies", "attach", "--dry-run"}, {"dependencies", "list", "--layout"},
	} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 || !strings.HasPrefix(errs.String(), "Error: ") {
			t.Fatalf("%v: exit %d stdout %s stderr %s", args, code, &out, &errs)
		}
	}
	opts, err := parseCommand("dependencies attach", []string{"example.com/a:v1", "-fdeps.yaml", "-q", "--format=json"})
	if err != nil || opts.file != "deps.yaml" || !opts.quiet || opts.format != "json" || len(opts.args) != 1 {
		t.Fatalf("%+v: %v", opts, err)
	}
	opts, err = parseCommand("dependencies list", []string{"example.com/a:v1", "-f{{.Digest}}"})
	if err != nil || opts.format != "{{.Digest}}" {
		t.Fatalf("%+v: %v", opts, err)
	}
	opts, err = parseCommand("dependencies inspect", []string{"example.com/a:v1", "-f{{.Declaration.Context}}", "--plain-http", "localhost:5000"})
	if err != nil || opts.format != "{{.Declaration.Context}}" || len(opts.args) != 1 || len(opts.plainHTTP) != 1 {
		t.Fatalf("%+v: %v", opts, err)
	}
}

func TestDependenciesInvalidInputBeforeRegistry(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	for _, raw := range []string{
		"dependencies: [", "dependencies: []\nunknown: true\n", "dependencies:\n  - ref: implicit/app:tag\n",
		"dependencies:\n  - ref: example.com/app:tag\n    follow: wrong\n",
	} {
		path := filepath.Join(t.TempDir(), "dependencies.yaml")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		var out, errs bytes.Buffer
		args := []string{"dependencies", "attach", host + "/subject:tag", "--plain-http", host, "-f", path}
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 {
			t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
		}
	}
	var out, errs bytes.Buffer
	if code := run([]string{"dependencies", "attach", host + "/subject:tag", "--plain-http", host, "-f", "/no/such/dependency-file"}, &out, &errs); code != 125 {
		t.Fatalf("exit %d", code)
	}
	for _, args := range [][]string{
		{"dependencies", "list", host + "/subject:tag", "--plain-http", host, "-f", "{{"},
		{"dependencies", "inspect", host + "/subject:tag", "--plain-http", host, "-f", "{{"},
		{"dependencies", "attach", host + "/subject:tag", "--plain-http", host, "--format", "{{", "-f", "/no/such/dependency-file"},
	} {
		out.Reset()
		errs.Reset()
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 || !strings.Contains(errs.String(), "invalid --format") {
			t.Fatalf("invalid template: exit %d stdout %s stderr %s", code, &out, &errs)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid input made %d HTTP requests", requests.Load())
	}
}

func TestDependencyDescriptorOutput(t *testing.T) {
	descriptor := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, []byte("{}"))
	for _, tc := range []struct {
		format      string
		descriptors []v1.Descriptor
		want        string
	}{
		{"", nil, "[]\n"}, {"{{.Digest}}", []v1.Descriptor{descriptor}, descriptor.Digest.String() + "\n"},
	} {
		formatter, err := newOutputFormatter(tc.format, true)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := writeDependencyResults(&out, formatter, tc.descriptors); err != nil || out.String() != tc.want {
			t.Fatalf("output %q: %v", out.String(), err)
		}
		if err := writeDependencyResults(brokenWriter{}, formatter, tc.descriptors); err == nil {
			t.Fatal("ignored output error")
		}
	}
	formatter, err := newOutputFormatter("{{.Missing}}", true)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeDependencyResults(&out, formatter, []v1.Descriptor{descriptor}); err == nil || out.Len() != 0 {
		t.Fatalf("template failure wrote output %s: %v", &out, err)
	}
}

func TestDependencyInspectionOutput(t *testing.T) {
	descriptor := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, []byte("{}"))
	declarations := []bale.InspectedDeclaration{{Repository: "example.com/app", Subject: descriptor, Descriptor: descriptor, Declaration: bale.DependencyDeclaration{Context: "production", Dependencies: []bale.DependencyReference{{Ref: "unreachable.invalid/image:stable"}}}}}
	for _, tc := range []struct {
		format       string
		declarations []bale.InspectedDeclaration
		want         string
	}{
		{"", nil, "[]\n"},
		{"{{.Declaration.Context}} {{(index .Declaration.Dependencies 0).Ref}}", declarations, "production unreachable.invalid/image:stable\n"},
		{"{{.Descriptor.Digest}}", append(declarations, declarations[0]), strings.Repeat(descriptor.Digest.String()+"\n", 2)},
	} {
		formatter, err := newOutputFormatter(tc.format, true)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := writeDependencyResults(&out, formatter, tc.declarations); err != nil || out.String() != tc.want {
			t.Fatalf("output %q: %v", out.String(), err)
		}
		if err := writeDependencyResults(brokenWriter{}, formatter, tc.declarations); err == nil {
			t.Fatal("ignored output error")
		}
	}
	formatter, err := newOutputFormatter("{{.Missing}}", true)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeDependencyResults(&out, formatter, declarations); err == nil || out.Len() != 0 {
		t.Fatalf("template failure wrote output %s: %v", &out, err)
	}
}

func TestDependenciesInspectEmptyRegistry(t *testing.T) {
	raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`)
	descriptor := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, raw)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead && r.Method != http.MethodGet {
			t.Errorf("unexpected registry write %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/app/manifests/"):
			w.Header().Set("Content-Type", descriptor.MediaType)
			w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
			w.Header().Set("Docker-Content-Digest", descriptor.Digest.String())
			if r.Method == http.MethodGet {
				_, _ = w.Write(raw)
			}
		case r.URL.Path == "/v2/app/referrers/"+descriptor.Digest.String():
			w.Header().Set("Content-Type", v1.MediaTypeImageIndex)
			_, _ = io.WriteString(w, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
		default:
			t.Errorf("unexpected registry lookup %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	for _, action := range []string{"inspect", "list", "ls"} {
		args := []string{"dependencies", action, host + "/app:stable", "--plain-http", host}
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 0 || out.String() != "[]\n" || errs.Len() != 0 {
			t.Fatalf("%s: exit %d stdout %s stderr %s", action, code, &out, &errs)
		}
		if code := run(args, brokenWriter{}, &errs); code != 125 || !strings.Contains(errs.String(), "write failed") {
			t.Fatalf("output error exit %d stderr %s", code, &errs)
		}
	}
}
