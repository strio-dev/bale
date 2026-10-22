package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
)

func TestDestinationsHelpAndFlags(t *testing.T) {
	for _, args := range [][]string{
		{"destinations"}, {"destinations", "--help"}, {"help", "destinations"},
		{"destinations", "inspect", "--help"}, {"help", "destinations", "inspect"},
	} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 || !strings.Contains(out.String(), "bale destinations") {
			t.Fatalf("%v: exit %d stdout %s stderr %s", args, code, &out, &errs)
		}
		if code := run(args, brokenWriter{}, &errs); code != 125 {
			t.Fatalf("output failure exit %d", code)
		}
	}
	for _, args := range [][]string{
		{"destinations", "other"}, {"destinations", "inspect"},
		{"destinations", "inspect", "example.com/app:v1", "extra"},
		{"destinations", "inspect", "example.com/app:v1", "-fjson", "--format=json"},
		{"destinations", "inspect", "example.com/app:v1", "--quiet"},
		{"destinations", "inspect", "example.com/app:v1", "--layout"},
		{"destinations", "inspect", "example.com/app:v1", "--select", "production"},
	} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 || !strings.HasPrefix(errs.String(), "Error: ") {
			t.Fatalf("%v: exit %d stdout %s stderr %s", args, code, &out, &errs)
		}
	}
	opts, err := parseCommand("destinations inspect", []string{"example.com/app:v1", "-f{{.Record.Bundle}}", "--plain-http", "localhost:5000"})
	if err != nil || opts.format != "{{.Record.Bundle}}" || len(opts.args) != 1 || len(opts.plainHTTP) != 1 {
		t.Fatalf("%+v: %v", opts, err)
	}
}

func TestDestinationsInvalidInputBeforeRegistry(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	for _, args := range [][]string{
		{"destinations", "inspect", host + "/app:v1", "--plain-http", host, "-f", "{{"},
		{"destinations", "inspect", host + "/app", "--plain-http", host},
		{"destinations", "inspect", host + "/app:v1", "--plain-http", "http://" + host},
	} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 {
			t.Fatalf("%v: exit %d stdout %s stderr %s", args, code, &out, &errs)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid input made %d HTTP requests", requests.Load())
	}
}

func TestDestinationInspectionOutput(t *testing.T) {
	d := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, []byte("{}"))
	records := []bale.InspectedDestination{{Local: true, Record: bale.DestinationRecord{Bundle: "example.com/bundle@" + d.Digest.String()}}}
	for _, tc := range []struct {
		format string
		rows   []bale.InspectedDestination
		want   string
	}{
		{"", nil, "[]\n"},
		{"{{.Local}} {{.Record.Bundle}}", records, "true example.com/bundle@" + d.Digest.String() + "\n"},
	} {
		formatter, err := newOutputFormatter(tc.format, true)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := writeDependencyResults(&out, formatter, tc.rows); err != nil || out.String() != tc.want {
			t.Fatalf("output %q: %v", out.String(), err)
		}
		if err := writeDependencyResults(brokenWriter{}, formatter, tc.rows); err == nil {
			t.Fatal("ignored output error")
		}
	}
	formatter, err := newOutputFormatter("{{.Local}}{{.Missing}}", true)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeDependencyResults(&out, formatter, records); err == nil || out.Len() != 0 {
		t.Fatalf("template failure wrote output %s: %v", &out, err)
	}
}

func TestDestinationsInspectEmptyRegistry(t *testing.T) {
	raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`)
	d := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, raw)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead && r.Method != http.MethodGet {
			t.Errorf("unexpected registry write %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/app/manifests/"):
			w.Header().Set("Content-Type", d.MediaType)
			w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
			w.Header().Set("Docker-Content-Digest", d.Digest.String())
			if r.Method == http.MethodGet {
				_, _ = w.Write(raw)
			}
		case r.URL.Path == "/v2/app/referrers/"+d.Digest.String():
			w.Header().Set("Content-Type", v1.MediaTypeImageIndex)
			_, _ = io.WriteString(w, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
		default:
			t.Errorf("unexpected registry lookup %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	args := []string{"destinations", "inspect", host + "/app:stable", "--plain-http", host}
	var out, errs bytes.Buffer
	if code := run(args, &out, &errs); code != 0 || out.String() != "[]\n" || errs.Len() != 0 {
		t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
	}
	if code := run(args, brokenWriter{}, &errs); code != 125 || !strings.Contains(errs.String(), "write failed") {
		t.Fatalf("output failure exit %d stderr %s", code, &errs)
	}
}
