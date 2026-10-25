package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/strio-dev/bale"
)

func TestLocalCreateUsesAuthenticatedSources(t *testing.T) {
	fixture, _, _ := cliBundle(t)
	client, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := client.InspectLayout(t.Context(), fixture)
	if err != nil {
		t.Fatal(err)
	}
	member := inspected.Metadata.Roots[0].Descriptor
	var mutations, authorized atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			mutations.Add(1)
			http.Error(w, "read-only source", http.StatusForbidden)
			return
		}
		if user, password, ok := r.BasicAuth(); !ok || user != "capture" || password != "fixture" {
			w.Header().Set("WWW-Authenticate", `Basic realm="capture"`)
			http.Error(w, "credentials required", http.StatusUnauthorized)
			return
		}
		authorized.Add(1)
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if name == "stable" {
			name = member.Digest.String()
		}
		dgst, err := digest.Parse(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		raw, err := os.ReadFile(filepath.Join(fixture, "blobs", dgst.Algorithm().String(), dgst.Encoded()))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeLayoutResponse(w, r, raw, name)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	credentials := t.TempDir()
	t.Setenv("DOCKER_CONFIG", credentials)
	config := fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, base64.StdEncoding.EncodeToString([]byte("capture:fixture")))
	if err := os.WriteFile(filepath.Join(credentials, "config.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	recipe := filepath.Join(dir, "bale.yaml")
	data := fmt.Sprintf("include:\n  - ref: %s/app:stable\n    dependencies:\n      follow: none\n    referrers:\n      include: none\n", host)
	if err := os.WriteFile(recipe, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	layout, delivery := filepath.Join(dir, "release"), filepath.Join(dir, "delivery.yaml")
	var out, errs bytes.Buffer
	args := []string{"create", "--layout", "-f", recipe, layout, "--output-delivery", delivery, "--format", "json", "--quiet", "--plain-http", host}
	if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
		t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
	}
	var result bale.CreateResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Published || result.DryRun || result.LayoutPath != layout || result.Delivery == nil || result.Delivery.Bundle != result.Descriptor.Digest {
		t.Fatalf("invalid local result %s: %v", &out, err)
	}
	if restored, err := client.InspectLayout(t.Context(), layout); err != nil || restored.Descriptor.Digest != result.Descriptor.Digest {
		t.Fatalf("local output is incomplete: %v", err)
	}
	if raw, err := os.ReadFile(delivery); err != nil || !bytes.Contains(raw, []byte(result.Descriptor.Digest)) {
		t.Fatalf("delivery output missing captured identity: %v", err)
	}
	out.Reset()
	errs.Reset()
	dryPath := filepath.Join(dir, "preview")
	if code := run([]string{"create", "--layout", "-f", recipe, dryPath, "--dry-run", "--format", "json", "--quiet", "--plain-http", host}, &out, &errs); code != 0 {
		t.Fatalf("dry run: exit %d stderr %s", code, &errs)
	}
	var preview bale.CreateResult
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil || !preview.DryRun || preview.LayoutPath != "" || preview.Descriptor.Digest != result.Descriptor.Digest {
		t.Fatalf("dry run capture differs: %s %v", &out, err)
	}
	if _, err := os.Lstat(dryPath); !os.IsNotExist(err) {
		t.Fatal("dry run published a layout")
	}
	if mutations.Load() != 0 || authorized.Load() == 0 {
		t.Fatalf("local capture source policy: mutations=%d authorized=%d", mutations.Load(), authorized.Load())
	}
	// Explicit auth selection and a stdin recipe also work through the actual
	// authenticated registry path, independently of Docker's ambient config.
	authFile := filepath.Join(credentials, "auth.json")
	if err := os.WriteFile(authFile, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentials, "config.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	withInput(t, data)
	out.Reset()
	errs.Reset()
	if code := run([]string{"create", "--store", t.TempDir(), "-f", "-", "--authfile", authFile, "--quiet", "--plain-http", host}, &out, &errs); code != 0 || errs.Len() != 0 {
		t.Fatalf("authenticated stdin create: exit %d stdout %s stderr %s", code, &out, &errs)
	}
}

func TestArchiveCLIIsLocal(t *testing.T) {
	layout, root, _ := cliBundle(t)
	credentials := t.TempDir()
	t.Setenv("DOCKER_CONFIG", credentials)
	if err := os.WriteFile(filepath.Join(credentials, "config.json"), []byte("invalid credentials JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archive, restored := filepath.Join(dir, "release.tar"), filepath.Join(dir, "restored")
	for _, args := range [][]string{
		{"save", "--layout", layout, "-o", archive, "--format", "json"},
		{"load", "--layout", "-i", archive, restored, "--format", "json"},
	} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
			t.Fatalf("%v: exit %d stdout %s stderr %s", args, code, &out, &errs)
		}
		var report bale.ArchiveResult
		if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Descriptor.Digest != root.Digest {
			t.Fatalf("archive report %s: %v", &out, err)
		}
	}
	client, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	comparison, err := client.DiffLayouts(t.Context(), layout, restored)
	if err != nil || comparison.Changed {
		t.Fatalf("archive changed captured inventory: %+v %v", comparison, err)
	}
	for _, args := range [][]string{
		{"save", "--layout", layout, "-o", archive},
		{"load", "--layout", "-i", archive, restored},
	} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 || !strings.Contains(errs.String(), "already exists") {
			t.Fatalf("existing output overwritten: exit %d stdout %s stderr %s", code, &out, &errs)
		}
	}
	var out, errs bytes.Buffer
	if code := run([]string{"save", "--layout", restored, "-o", filepath.Join(dir, "second.tar")}, &out, &errs); code != 0 || out.String() != root.Digest.String()+"\n" {
		t.Fatalf("default save output: exit %d stdout %s stderr %s", code, &out, &errs)
	}
}

func TestArchiveCommandValidation(t *testing.T) {
	for _, args := range [][]string{
		{"save"}, {"save", "--layout", "layout"}, {"save", "--layout", "layout", "-o", ""},
		{"save", "--layout", "layout", "-o", "out", "--output", "other"},
		{"load"}, {"load", "--layout", "layout"}, {"load", "--layout", "-i", "", "layout"},
		{"load", "--layout", "-i", "archive", "one", "two"},
		{"load", "--layout", "-i", "archive", "layout", "--plain-http", "localhost:5000"},
	} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 || !strings.HasPrefix(errs.String(), "Error:") {
			t.Fatalf("%v: exit %d stdout %s stderr %s", args, code, &out, &errs)
		}
	}
	for _, command := range []string{"save", "load"} {
		var out, errs bytes.Buffer
		if code := run([]string{"help", command}, &out, &errs); code != 0 || errs.Len() != 0 || !strings.Contains(out.String(), "bale "+command+" [options]") || strings.Contains(out.String(), "--plain-http") != (command == "save") {
			t.Fatalf("%s help: exit %d stdout %s stderr %s", command, code, &out, &errs)
		}
	}
}

func TestMetadataCreateDefersPayloadsUntilSave(t *testing.T) {
	fixture, _, payload := cliBundle(t)
	client, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := client.InspectLayout(t.Context(), fixture)
	if err != nil {
		t.Fatal(err)
	}
	member := inspection.Metadata.Roots[0].Descriptor
	var requests, mutations, payloadReads atomic.Int32
	var rejectTag atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			mutations.Add(1)
			http.Error(w, "read-only source", http.StatusForbidden)
			return
		}
		if user, password, ok := r.BasicAuth(); !ok || user != "capture" || password != "fixture" {
			w.Header().Set("WWW-Authenticate", `Basic realm="capture"`)
			http.Error(w, "credentials required", http.StatusUnauthorized)
			return
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if name == "stable" {
			if rejectTag.Load() {
				http.NotFound(w, r)
				return
			}
			name = member.Digest.String()
		}
		dgst, err := digest.Parse(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet && dgst == payload.Digest {
			payloadReads.Add(1)
		}
		raw, err := os.ReadFile(filepath.Join(fixture, "blobs", dgst.Algorithm().String(), dgst.Encoded()))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeLayoutResponse(w, r, raw, name)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	credentials := t.TempDir()
	t.Setenv("DOCKER_CONFIG", credentials)
	config := fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, base64.StdEncoding.EncodeToString([]byte("capture:fixture")))
	configPath := filepath.Join(credentials, "config.json")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	recipe, capture := filepath.Join(dir, "bale.yaml"), "capture:v1"
	storePath := filepath.Join(dir, "store")
	data := fmt.Sprintf("include:\n  - ref: %s/app:stable\n    dependencies:\n      follow: none\n    referrers:\n      include: none\n", host)
	if err := os.WriteFile(recipe, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) []byte {
		t.Helper()
		var out, errs bytes.Buffer
		if code := run(append(args, "--store", storePath), &out, &errs); code != 0 || errs.Len() != 0 {
			t.Fatalf("%v: exit %d stdout %s stderr %s", args, code, &out, &errs)
		}
		return out.Bytes()
	}
	preview := invoke("create", "-f", recipe, "--dry-run", "--plain-http", host, "--quiet")
	if !strings.Contains(string(preview), "payloads remain at pinned sources") {
		t.Fatalf("create preview hid source dependence: %s", preview)
	}
	if listing := invoke("ls", "--format=json"); strings.TrimSpace(string(listing)) != "[]" {
		t.Fatalf("preview retained a bundle: %s", listing)
	}
	createdJSON := invoke("create", "-f", recipe, "-t", capture, "--plain-http", host, "--format=json", "--quiet")
	var created bale.CreateResult
	if err := json.Unmarshal(createdJSON, &created); err != nil || !created.MetadataOnly || created.Published || created.LayoutPath != "" {
		t.Fatalf("metadata create %s: %v", createdJSON, err)
	}
	if payloadReads.Load() != 0 || mutations.Load() != 0 {
		t.Fatalf("creation transferred payloads or wrote sources: payloads=%d mutations=%d", payloadReads.Load(), mutations.Load())
	}
	if _, err := os.Stat(filepath.Join(storePath, "blobs", payload.Digest.Algorithm().String(), payload.Digest.Encoded())); !os.IsNotExist(err) {
		t.Fatalf("metadata capture retained an opaque payload: %v", err)
	}
	// Neither local inspection nor planning may consult sources or credentials.
	before := requests.Load()
	if err := os.WriteFile(configPath, []byte("invalid credentials JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	inspectedJSON := invoke("inspect", capture)
	var inspected []bale.InspectResult
	if err := json.Unmarshal(inspectedJSON, &inspected); err != nil || len(inspected) != 1 || !inspected[0].MetadataOnly || inspected[0].Descriptor.Digest != created.Descriptor.Digest {
		t.Fatalf("metadata inspection %s: %v", inspectedJSON, err)
	}
	preview = invoke("push", capture, "destination.invalid/release:bundle", "--dry-run", "--quiet")
	if !strings.Contains(string(preview), "actual push needs source access") || requests.Load() != before {
		t.Fatalf("metadata preview hid source dependence or accessed sources: %s", preview)
	}
	rejectTag.Store(true)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "capture.tar")
	invoke("save", capture, "-o", archive, "--plain-http", host, "--quiet")
	if payloadReads.Load() == 0 || mutations.Load() != 0 {
		t.Fatalf("save did not materialize read-only payloads: payloads=%d mutations=%d", payloadReads.Load(), mutations.Load())
	}
	server.Close()
	if err := os.WriteFile(configPath, []byte("invalid credentials JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	invoke("load", "-i", archive, "-t", "restored:v1", "--quiet")
	local, err := openManaged(t.Context(), commandOptions{storePath: storePath}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := local.Inspect(t.Context(), "restored:v1")
	if err != nil || result.MetadataOnly || result.Descriptor.Digest != created.Descriptor.Digest {
		t.Fatalf("saved capture is not a complete pinned offline bundle: %+v %v", result, err)
	}
	invoke("tag", capture, "approved:v1")
	invoke("inspect", "approved:v1")
	var selected, selectedErrors bytes.Buffer
	if code := run([]string{"--store", storePath, "inspect", "--", "approved:v1"}, &selected, &selectedErrors); code != 0 || !strings.Contains(selected.String(), created.Descriptor.Digest.String()) {
		t.Fatalf("global store after terminator: code=%d output=%s error=%s", code, &selected, &selectedErrors)
	}
	invoke("rm", capture)
	invoke("inspect", created.Descriptor.Digest.String())
	invoke("rm", created.Descriptor.Digest.String())
	if listing := invoke("ls", "--format=json"); strings.TrimSpace(string(listing)) != "[]" {
		t.Fatalf("removed root retained: %s", listing)
	}
	invoke("prune", "--dry-run")

}
