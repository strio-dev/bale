package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
)

// Extend the ordinary offline fixture with a second source and a captured
// attachment. Splitting their primary paths still requires colocated copies.
func pushPreviewBundle(t *testing.T) (string, bale.InspectResult, v1.Descriptor) {
	t.Helper()
	dir, _, payload := cliBundle(t)
	client, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := client.InspectLayout(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := oci.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	put := func(value any) v1.Descriptor {
		raw, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		desc := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, raw)
		if pushErr := store.Push(t.Context(), desc, bytes.NewReader(raw)); pushErr != nil {
			t.Fatal(pushErr)
		}
		return desc
	}
	member := inspected.Metadata.Roots[0].Descriptor
	raw, err := os.ReadFile(filepath.Join(dir, "blobs", member.Digest.Algorithm().String(), member.Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Annotations = map[string]string{"example": "second artifact"}
	second := put(manifest)
	attachment := put(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: "application/vnd.example.signature", Subject: &member, Config: manifest.Config, Layers: []v1.Descriptor{payload}})
	m := inspected.Metadata
	m.Recipe.Include[0].Referrers = config.Policy{Include: "all"}
	selection := m.Recipe.Include[0]
	selection.Ref = "other.invalid/app:stable"
	selection.Referrers = config.Policy{Include: "none"}
	m.Recipe.Include = append(m.Recipe.Include, selection)
	m.Roots = append(m.Roots, bundle.Root{Input: 1, Repository: "other.invalid/app", Reference: "stable", Descriptor: second})
	m.Origins = append(m.Origins, bundle.Origin{Repository: "other.invalid/app", Descriptor: second}, bundle.Origin{Repository: "unreachable.invalid/app", Descriptor: attachment})
	m.Referrers = append(m.Referrers, bundle.Association{Repository: "unreachable.invalid/app", Subject: member, Descriptor: attachment})
	observation := m.Dependencies[0]
	observation.Repository, observation.Subject = "other.invalid/app", second
	m.Dependencies = append(m.Dependencies, observation)
	snapshot, err := bundle.BuildSnapshot(t.Context(), store, m)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{snapshot.Descriptor}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	inspected, err = client.InspectLayout(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, inspected, payload
}

func TestPushPreviewMatchesSDKWithoutNetworkOrCredentials(t *testing.T) {
	dir, inspected, _ := pushPreviewBundle(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "remote access forbidden", http.StatusForbidden)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	destination := host + "/releases/v1:bundle"
	docker := t.TempDir()
	if err := os.WriteFile(filepath.Join(docker, "config.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", docker)
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	member := inspected.Metadata.Roots[0].Descriptor
	file := bale.DeliveryFile{Bundle: inspected.Descriptor.Digest, Artifacts: []bale.DeliveryArtifact{{Ref: "unreachable.invalid/app@" + member.Digest.String(), Path: "applications/web"}}}
	raw, err := bale.EncodeDeliveryFile(file)
	if err != nil {
		t.Fatal(err)
	}
	deliveryPath := filepath.Join(t.TempDir(), "delivery.yaml")
	if err := os.WriteFile(deliveryPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	// No planning step needs a writable layout, including OCI framing files.
	makePushPreviewLayoutReadOnly(t, dir)
	opts := bale.PushOptions{RepositoryMap: map[string]string{"unreachable.invalid/app": "attachments", "other.invalid/app": "vendor/app"}, Delivery: &file}
	expected, err := client.PlanPush(t.Context(), dir, destination, opts)
	if err != nil {
		t.Fatal(err)
	}
	before := layoutBytes(t, dir)
	args := []string{"push", dir, destination, "--dry-run", "--plain-http", host, "--delivery", deliveryPath, "--map", "unreachable.invalid/app=attachments", "--map", "other.invalid/app=vendor/app", "--format", "json"}
	var out, errs bytes.Buffer
	if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
		t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
	}
	var actual bale.DeliveryPlan
	if err := json.Unmarshal(out.Bytes(), &actual); err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(actual)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Fatalf("CLI plan differs from SDK:\ngot %s\nwant %s", gotJSON, wantJSON)
	}
	if actual.Destination != destination || actual.Descriptor.Digest != file.Bundle || len(actual.Tags) != 2 || len(actual.Destinations) != 3 || len(actual.Relocations) != 3 {
		t.Fatalf("incomplete plan: %+v", actual)
	}
	for _, tag := range actual.Tags {
		if tag.Reference != "stable" || tag.Repository == host+"/releases/v1" {
			t.Fatalf("completion tag mixed with restored aliases: %+v", actual.Tags)
		}
	}
	placement, ok := actual.Relocations.Lookup(bale.ArtifactID{Repository: "unreachable.invalid/app", Digest: member.Digest})
	if !ok || placement.Destination.Repository != host+"/releases/v1/applications/web" {
		t.Fatalf("delivery override lost: %+v", actual.Relocations)
	}
	for _, generated := range actual.Destinations {
		if generated.Descriptor.Digest == "" || generated.Subject.Digest == "" || !strings.HasPrefix(generated.Record.Destination, generated.Repository+"@") {
			t.Fatalf("missing generated attachment identities: %+v", generated)
		}
	}
	// Each independently placed attachment must retain its subject beside it.
	foundExtraSubject := false
	for _, repository := range actual.Repositories {
		if repository.Repository == host+"/releases/v1/attachments" {
			for _, root := range repository.Roots {
				foundExtraSubject = foundExtraSubject || root.Digest == member.Digest
			}
		}
	}
	if !foundExtraSubject || len(actual.Referrers) < 2 {
		t.Fatalf("mandatory colocated copies lost: %+v", actual)
	}
	if requests.Load() != 0 {
		t.Fatalf("preview made %d remote requests", requests.Load())
	}
	if !reflect.DeepEqual(before, layoutBytes(t, dir)) {
		t.Fatal("preview modified the captured layout")
	}
	if after, err := os.ReadFile(deliveryPath); err != nil || !bytes.Equal(after, raw) {
		t.Fatalf("preview modified the delivery file: %v", err)
	}
}

func makePushPreviewLayoutReadOnly(t *testing.T, dir string) {
	t.Helper()
	permissions := func(readOnly bool) error {
		return filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			mode := os.FileMode(0600)
			if readOnly {
				mode = 0400
			}
			if entry.IsDir() {
				mode |= 0100
			}
			return os.Chmod(path, mode)
		})
	}
	t.Cleanup(func() {
		if err := permissions(false); err != nil {
			t.Error(err)
		}
	})
	if err := permissions(true); err != nil {
		t.Fatal(err)
	}
}

func TestPushPreviewLocalFailuresDoNotPublish(t *testing.T) {
	for _, name := range []string{"missing payload", "corrupt payload", "stale delivery", "unknown identity", "unknown mapping", "mapping collision", "tag collision"} {
		t.Run(name, func(t *testing.T) {
			dir, inspected, payload := pushPreviewBundle(t)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				http.Error(w, "remote access forbidden", http.StatusForbidden)
			}))
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "http://")
			args := []string{"push", dir, host + "/release:bundle", "--dry-run", "--plain-http", host}
			file := bale.DeliveryFile{Bundle: inspected.Descriptor.Digest, Artifacts: []bale.DeliveryArtifact{}}
			switch name {
			case "missing payload", "corrupt payload":
				path := filepath.Join(dir, "blobs", payload.Digest.Algorithm().String(), payload.Digest.Encoded())
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if name == "corrupt payload" {
					if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(payload.Size)), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "stale delivery":
				file.Bundle = digest.FromString("stale capture")
			case "unknown identity":
				file.Artifacts = []bale.DeliveryArtifact{{Ref: "unknown.invalid/app@" + inspected.Metadata.Roots[0].Descriptor.Digest.String(), Path: "elsewhere"}}
			case "unknown mapping":
				args = append(args, "--map", "unknown.invalid/app=app")
			case "mapping collision":
				args = append(args, "--map", "unreachable.invalid/app=shared", "--map", "other.invalid/app=shared")
			case "tag collision":
				for _, root := range inspected.Metadata.Roots {
					file.Artifacts = append(file.Artifacts, bale.DeliveryArtifact{Ref: root.Repository + "@" + root.Descriptor.Digest.String(), Path: "shared"})
				}
			}
			if name == "stale delivery" || name == "unknown identity" || name == "tag collision" {
				raw, err := bale.EncodeDeliveryFile(file)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "delivery.yaml")
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--delivery", path)
			}
			before := layoutBytes(t, dir)
			var out, errs bytes.Buffer
			if code := run(args, &out, &errs); code != 125 || out.Len() != 0 || !strings.HasPrefix(errs.String(), "Error: ") {
				t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
			}
			if requests.Load() != 0 || !reflect.DeepEqual(before, layoutBytes(t, dir)) {
				t.Fatalf("failed preview made %d requests or modified layout", requests.Load())
			}
		})
	}
}

func TestPushWithoutPreviewStillChecksDestination(t *testing.T) {
	dir, _, _ := cliBundle(t)
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			t.Errorf("write attempted despite denied preflight: %s %s", r.Method, r.URL)
		}
		http.Error(w, "destination denied", http.StatusForbidden)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	for _, flags := range [][]string{nil, {"--dry-run=false"}} {
		requests.Store(0)
		args := append([]string{"push", dir, host + "/release:bundle", "--plain-http", host}, flags...)
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 125 || out.Len() != 0 || errs.Len() == 0 {
			t.Fatalf("%v: exit %d stdout %s stderr %s", flags, code, &out, &errs)
		}
		if requests.Load() == 0 {
			t.Fatalf("%v: ordinary push skipped destination checks", flags)
		}
	}
}
