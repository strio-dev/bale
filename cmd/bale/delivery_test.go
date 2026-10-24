package main

import (
	"bytes"
	"fmt"
	"github.com/opencontainers/go-digest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/strio-dev/bale"
)

func TestDeliveryExportLocal(t *testing.T) {
	dir, root, _ := cliBundle(t)
	before := layoutBytes(t, dir)
	output := filepath.Join(t.TempDir(), "delivery.yaml")
	var out, errs bytes.Buffer
	if code := run([]string{"inspect", "--layout", dir, "--output-delivery", output}, &out, &errs); code != 0 || out.Len() != 0 || errs.Len() != 0 {
		t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := bale.DecodeDeliveryFile(raw)
	if err != nil || delivery.Bundle != root.Digest || len(delivery.Artifacts) != 1 || delivery.Artifacts[0].Path != "app" {
		t.Fatalf("delivery %s: %v", raw, err)
	}
	after := layoutBytes(t, dir)
	if len(before) != len(after) {
		t.Fatal("layout changed")
	}
	for path, data := range before {
		if data != after[path] {
			t.Fatalf("layout changed: %s", path)
		}
	}
}

func TestDeliveryOutputPreservesExistingAndConcurrentFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delivery.yaml")
	output, err := prepareDeliveryOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	defer output.close()
	original := []byte("edited delivery file")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	dir, _, _ := cliBundle(t)
	client, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := client.ExportDeliveryLayout(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := output.write(delivery); err == nil {
		t.Fatal("overwrote concurrent file")
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("file changed: %s %v", actual, err)
	}
	if _, err := prepareDeliveryOutput(path); err == nil {
		t.Fatal("accepted existing file")
	}
}

func TestDeliveryCLIValidationBeforeStorage(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "recipe.yaml")
	if err := os.WriteFile(existing, []byte("recipe"), 0600); err != nil {
		t.Fatal(err)
	}
	malformed := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(malformed, []byte("unknown: value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"inspect", "--layout", "missing", "--output-delivery="}, "nonempty path"},
		{[]string{"push", "--layout", "missing", "unreachable.invalid/b:tag", "--delivery="}, "nonempty path"},
		{[]string{"inspect", "--layout", "missing", "other", "--output-delivery", "ignored.yaml"}, "requires one bundle"},
		{[]string{"inspect", "--layout", "missing", "--output-delivery", "ignored.yaml", "--format", "json"}, "cannot be combined"},
		{[]string{"create", "--registry", "-f", existing, "unreachable.invalid/b:tag", "--output-delivery", existing}, "already exists"},
		{[]string{"inspect", "--layout", "missing", "--output-delivery", existing}, "already exists"},
		{[]string{"push", "--layout", "missing", "unreachable.invalid/b:tag", "--delivery", malformed}, "unknown"},
		{[]string{"push", "--layout", "missing", "unreachable.invalid/b:tag", "--delivery", "missing-delivery-file"}, "no such file"},
		{[]string{"inspect", "--layout", "missing", "--output-delivery", filepath.Join(t.TempDir(), "absent", "file.yaml")}, "no such file"},
	} {
		var out, errs bytes.Buffer
		if code := run(tc.args, &out, &errs); code != 125 || out.Len() != 0 || !strings.Contains(errs.String(), tc.want) {
			t.Fatalf("%v: exit %d stdout %s stderr %s; want %s", tc.args, code, &out, &errs, tc.want)
		}
	}
}

func TestDeliveryExportFailureRemovesTemporaryFile(t *testing.T) {
	outputDir := t.TempDir()
	var out, errs bytes.Buffer
	if code := run([]string{"inspect", "--layout", "missing-layout", "--output-delivery", filepath.Join(outputDir, "delivery.yaml")}, &out, &errs); code != 125 {
		t.Fatalf("exit %d stderr %s", code, &errs)
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("left output files: %v %v", entries, err)
	}
}

func TestCreateDryRunExportsExactCapture(t *testing.T) {
	dir, _, _ := cliBundle(t)
	client, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := client.InspectLayout(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	member := inspection.Metadata.Roots[0].Descriptor
	var resolutions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			t.Errorf("dry run write: %s %s", r.Method, r.URL)
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if name == "stable" {
			resolutions.Add(1)
			name = member.Digest.String()
		}
		dgst, err := digest.Parse(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		raw, err := os.ReadFile(filepath.Join(dir, "blobs", dgst.Algorithm().String(), dgst.Encoded()))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeLayoutResponse(w, r, raw, name)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	recipe := filepath.Join(t.TempDir(), "bale.yaml")
	raw := fmt.Sprintf("include:\n  - ref: %s/app:stable\n    dependencies:\n      follow: none\n    referrers:\n      include: none\n", host)
	if err := os.WriteFile(recipe, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "delivery.yaml")
	var out, errs bytes.Buffer
	args := []string{"create", "--registry", "-f", recipe, host + "/bundles:release", "--plain-http", host, "--dry-run", "--quiet", "--output-delivery", output}
	if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
		t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
	}
	encoded, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := bale.DecodeDeliveryFile(encoded)
	if err != nil || !strings.Contains(out.String(), "Bundle: "+delivery.Bundle.String()+"\n") || !strings.Contains(out.String(), host+"/app@"+member.Digest.String()) || len(delivery.Artifacts) != 1 || delivery.Artifacts[0].Ref != host+"/app@"+member.Digest.String() {
		t.Fatalf("capture output %s yaml %s: %v", &out, encoded, err)
	}
	if resolutions.Load() != 1 {
		t.Fatalf("source tag resolved %d times", resolutions.Load())
	}
}

func TestPushRejectsStaleDeliveryBeforePublication(t *testing.T) {
	dir, _, _ := cliBundle(t)
	client, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := client.ExportDeliveryLayout(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	delivery.Bundle = digest.FromString("different bundle")
	raw, err := bale.EncodeDeliveryFile(delivery)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "delivery.yaml")
	if err := os.WriteFile(input, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	var out, errs bytes.Buffer
	if code := run([]string{"push", "--layout", dir, host + "/release:bundle", "--plain-http", host, "--delivery", input}, &out, &errs); code != 125 || out.Len() != 0 || !strings.Contains(errs.String(), "bundle") {
		t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
	}
	if requests.Load() != 0 {
		t.Fatalf("stale input made %d registry requests", requests.Load())
	}
}
