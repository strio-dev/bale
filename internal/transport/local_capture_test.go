package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
)

func TestCreateLocalRetainsMetadataWithoutPayloadDownloads(t *testing.T) {
	r, repo, s, writes := monitoredRegistry(t)
	subject := object(t, r.store, "subject layer")
	child := snapshotFor(t, r.store, []string{repo}, []v1.Descriptor{object(t, r.store, "nested layer")})
	image := object(t, r.store, "dependency layer")
	r.tags["subject"], r.tags["child"], r.tags["image"] = subject, child.Descriptor, image
	capturePluginFixture(t, r)
	calls := 0
	s.pluginRunner = func(context.Context, PluginRequest) (dependency.Declaration, error) {
		calls++
		return dependency.Declaration{Dependencies: []dependency.Reference{{Ref: repo + ":image"}}}, nil
	}
	selected := dependencySelection(repo + ":subject")
	selected.Plugin = &config.Plugin{Ref: repo + ":plugin", Input: json.RawMessage(`{}`)}
	recipe := config.Recipe{Include: []config.Selection{selected, dependencySelection(repo + ":child")}}
	path := filepath.Join(t.TempDir(), "capture")
	created, err := s.CreateLocal(ctx, recipe, path, CreateOptions{ExportDelivery: true})
	if err != nil {
		t.Fatal(err)
	}
	if !created.MetadataOnly || created.Published || created.LayoutPath != path || created.Delivery == nil || calls != 1 {
		t.Fatalf("incorrect capture: %+v calls=%d", created, calls)
	}
	if writes.Load() != 0 {
		t.Fatal("capture wrote to source registry")
	}
	// Discovery may read its module. Ordinary image layers remain absent locally.
	store, root, err := readLayoutRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []v1.Descriptor{subject, image} {
		raw, err := content.FetchAll(ctx, store, d)
		if err != nil {
			t.Fatal(err)
		}
		env, err := bundle.ParseEnvelope(d, raw)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := store.Exists(ctx, env.Children[1]); err != nil || ok {
			t.Fatalf("opaque payload retained: %t %v", ok, err)
		}
	}
	if _, err := bundle.LoadSnapshotGraph(ctx, store, root); err == nil {
		t.Fatal("metadata capture passed full closure verification")
	}
	// No network is needed for metadata inspection, nested metadata, delivery,
	// comparison, update, or planning. Make every HTTP request fail explicitly.
	offline, err := NewService(Options{HTTPClient: &http.Client{Transport: deliveryRoundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("network forbidden") })}})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := offline.InspectLayout(ctx, path)
	if err != nil || !inspection.MetadataOnly || len(inspection.Declarations) != 1 {
		t.Fatalf("offline inspection: %+v %v", inspection, err)
	}
	exported, err := offline.ExportDeliveryLayout(ctx, path)
	if err != nil || exported.Bundle != created.Descriptor.Digest {
		t.Fatalf("offline export: %+v %v", exported, err)
	}
	plan, err := offline.PlanPush(ctx, path, "destination.example/release:v1", PushOptions{Delivery: created.Delivery})
	if err != nil || !plan.MetadataOnly {
		t.Fatalf("offline plan: %+v %v", plan, err)
	}
	diff, err := offline.DiffLayouts(ctx, path, path)
	if err != nil || diff.Changed {
		t.Fatalf("offline diff: %+v %v", diff, err)
	}
	update, err := offline.UpdateDeliveryLayouts(ctx, path, path, exported)
	if err != nil || update.Delivery == nil {
		t.Fatalf("offline update: %+v %v", update, err)
	}
	if calls != 1 {
		t.Fatal("reading capture reran plugin")
	}
}

func TestCreateLocalOnlyReadsOpaquePayloadsDuringExplicitSave(t *testing.T) {
	r, repo, s, writes := monitoredRegistry(t)
	old := object(t, r.store, "original payload")
	r.tags["stable"] = old
	recipe := config.Recipe{Include: []config.Selection{dependencySelection(repo + ":stable")}}
	path := filepath.Join(t.TempDir(), "capture")
	created, err := s.CreateLocal(ctx, recipe, path, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.blobReads != 0 {
		t.Fatalf("metadata creation downloaded %d payloads", r.blobReads)
	}
	// Changing the tag after creation must not refresh the capture on save.
	r.tags["stable"] = object(t, r.store, "new payload")
	resolutions := r.headTags["stable"]
	archive := filepath.Join(t.TempDir(), "offline.tar")
	saved, err := s.Save(ctx, path, archive)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Descriptor.Digest != created.Descriptor.Digest || r.blobReads == 0 {
		t.Fatalf("save did not materialize pinned snapshot: %+v reads=%d", saved, r.blobReads)
	}
	if r.headTags["stable"] != resolutions || writes.Load() != 0 {
		t.Fatal("save refreshed source or wrote to registry")
	}
	restored := filepath.Join(t.TempDir(), "restored")
	offline, _ := NewService(Options{})
	loaded, err := offline.Load(ctx, archive, restored)
	if err != nil || loaded.Descriptor.Digest != created.Descriptor.Digest {
		t.Fatalf("offline restore: %+v %v", loaded, err)
	}
	inspected, err := offline.InspectLayout(ctx, restored)
	if err != nil || inspected.MetadataOnly || inspected.Metadata.Roots[0].Descriptor.Digest != old.Digest {
		t.Fatalf("restored capture differs: %+v %v", inspected, err)
	}
	if _, err := os.Stat(filepath.Join(restored, captureFile)); !os.IsNotExist(err) {
		t.Fatal("offline archive retained source-dependent marker")
	}
}

func TestLocalCaptureRejectsChangedLocatorsAndInvalidMetadata(t *testing.T) {
	r, repo, s, _ := monitoredRegistry(t)
	r.tags["stable"] = object(t, r.store, "layer")
	recipe := config.Recipe{Include: []config.Selection{dependencySelection(repo + ":stable")}}
	for _, scenario := range []string{"changed bytes", "missing locator", "unreachable digest", "unreachable repository", "duplicate key", "wrong bundle", "missing metadata"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "capture")
			created, err := s.CreateLocal(ctx, recipe, path, CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := s.PlanPush(ctx, path, "destination.example/release:v1", PushOptions{})
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(path, captureFile)
			raw, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			var state CaptureState
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "changed bytes":
				raw = append(raw, '\n')
			case "duplicate key":
				raw = []byte(`{"schemaVersion":1,"schemaVersion":1,"bundle":"` + created.Descriptor.Digest.String() + `","sources":{}}`)
			case "missing metadata":
				if err := os.Remove(filepath.Join(path, "blobs", created.Descriptor.Digest.Algorithm().String(), created.Descriptor.Digest.Encoded())); err != nil {
					t.Fatal(err)
				}
			default:
				for dgst := range state.Sources {
					delete(state.Sources, dgst)
					break
				}
				switch scenario {
				case "unreachable digest":
					state.Sources[digest.FromString("unreachable")] = repo
				case "unreachable repository":
					var original CaptureState
					_ = json.Unmarshal(raw, &original)
					for dgst := range original.Sources {
						state.Sources[dgst] = "unrelated.example/repo"
						break
					}
				case "wrong bundle":
					state.Bundle = digest.FromString("different bundle")
				}
				raw, err = json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(marker, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Push(ctx, plan); err == nil {
				t.Fatal("push accepted changed or invalid capture")
			}
			if scenario != "changed bytes" {
				if _, err := s.InspectLayout(ctx, path); err == nil {
					t.Fatal("inspection accepted invalid capture")
				}
			}
		})
	}
}

func TestLocalCaptureSaveRejectsMissingAndCorruptPinnedPayload(t *testing.T) {
	for _, scenario := range []string{"missing", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			r := &readRegistry{store: &testStore{metadataStore: newMetadataStore(), descriptors: map[string]v1.Descriptor{}}, tags: map[string]v1.Descriptor{}, refs: map[string][]v1.Descriptor{}, headTags: map[string]int{}}
			root := object(t, r.store, "payload")
			r.tags["stable"] = root
			raw, _ := content.FetchAll(ctx, r.store, root)
			env, _ := bundle.ParseEnvelope(root, raw)
			payload := env.Children[1]
			fail := atomic.Bool{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if fail.Load() && req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/blobs/"+payload.Digest.String()) {
					if scenario == "missing" {
						http.NotFound(w, req)
					} else {
						_, _ = io.WriteString(w, strings.Repeat("!", int(payload.Size)))
					}
					return
				}
				r.serve(w, req)
			}))
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "http://")
			s, _ := NewService(Options{PlainHTTPRegistries: []string{host}})
			path := filepath.Join(t.TempDir(), "capture")
			if _, err := s.CreateLocal(ctx, config.Recipe{Include: []config.Selection{dependencySelection(host + "/app:stable")}}, path, CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			fail.Store(true)
			archive := filepath.Join(t.TempDir(), "offline.tar")
			if _, err := s.Save(ctx, path, archive); err == nil {
				t.Fatal("saved incomplete or corrupt payload")
			}
			if _, err := os.Lstat(archive); !os.IsNotExist(err) {
				t.Fatalf("failed save published archive: %v", err)
			}
			if _, err := s.InspectLayout(ctx, path); err != nil {
				t.Fatalf("payload failure damaged capture: %v", err)
			}
		})
	}
}

func TestCreateLocalDryRunAndCompleteLayoutIntegrity(t *testing.T) {
	r, repo, s, _ := monitoredRegistry(t)
	r.tags["stable"] = object(t, r.store, "payload")
	recipe := config.Recipe{Include: []config.Selection{dependencySelection(repo + ":stable")}}
	path := filepath.Join(t.TempDir(), "capture")
	preview, err := s.CreateLocal(ctx, recipe, path, CreateOptions{DryRun: true, ExportDelivery: true})
	if err != nil || !preview.MetadataOnly || preview.LayoutPath != "" || preview.Delivery == nil {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("preview created local output")
	}
	if _, err := s.CreateLayout(ctx, recipe, path, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	store, snapshot, err := openLayout(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := content.FetchAll(ctx, store, snapshot.Metadata.Roots[0].Descriptor)
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	payload := manifest.Layers[0]
	if err := os.Remove(filepath.Join(path, "blobs", payload.Digest.Algorithm().String(), payload.Digest.Encoded())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InspectLayout(ctx, path); err == nil {
		t.Fatal("unmarked incomplete layout treated as metadata capture")
	}
	if _, err := s.PlanPush(ctx, path, "destination.example/release:v1", PushOptions{}); err == nil {
		t.Fatal("incomplete layout passed push validation")
	}
}

func TestCreateLocalOpaqueConfigWithEnvelopeMediaType(t *testing.T) {
	r, repo, s, _ := monitoredRegistry(t)
	cfg := put(t, r.store, v1.MediaTypeImageManifest, []byte("opaque config"))
	raw, _ := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{}})
	r.tags["stable"] = put(t, r.store, v1.MediaTypeImageManifest, raw)
	path := filepath.Join(t.TempDir(), "capture")
	if _, err := s.CreateLocal(ctx, config.Recipe{Include: []config.Selection{dependencySelection(repo + ":stable")}}, path, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if r.blobReads != 0 {
		t.Fatal("opaque config was interpreted or downloaded")
	}
	if _, err := s.Save(ctx, path, filepath.Join(t.TempDir(), "offline.tar")); err != nil {
		t.Fatal(err)
	}
}

func TestPushLocalCaptureStreamsPinnedPayloadAndReusesDestination(t *testing.T) {
	for _, scenario := range []string{"stream", "reuse", "missing", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			r, repo, s, _ := monitoredRegistry(t)
			original := object(t, r.store, "pinned payload")
			r.tags["stable"] = original
			path := filepath.Join(t.TempDir(), "capture")
			created, err := s.CreateLocal(ctx, config.Recipe{Include: []config.Selection{dependencySelection(repo + ":stable")}}, path, CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := content.FetchAll(ctx, r.store, original)
			env, _ := bundle.ParseEnvelope(original, raw)
			payload := env.Children[1]
			r.tags["stable"] = object(t, r.store, "changed tag payload")
			resolutions := r.headTags["stable"]
			target := &directRegistry{objects: map[string][]byte{}, types: map[string]string{}}
			server := httptest.NewServer(http.HandlerFunc(target.serve))
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "http://")
			s.httpHosts[host] = true
			destination := host + "/release:v1"
			plan, err := s.PlanPush(ctx, path, destination, PushOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if r.blobReads != 0 {
				t.Fatal("planning downloaded payload")
			}
			switch scenario {
			case "reuse":
				bytes, err := content.FetchAll(ctx, r.store, payload)
				if err != nil {
					t.Fatal(err)
				}
				target.objects["release|blobs|"+payload.Digest.String()] = bytes
				for _, entry := range plan.Repositories {
					name := strings.TrimPrefix(entry.Repository, host+"/")
					target.objects[name+"|blobs|"+payload.Digest.String()] = bytes
				}
				delete(r.store.descriptors, payload.Digest.String())
			case "missing":
				delete(r.store.descriptors, payload.Digest.String())
			case "corrupt":
				s.client.Client = &http.Client{Transport: deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
					if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/blobs/"+payload.Digest.String()) {
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/octet-stream"}}, Body: io.NopCloser(strings.NewReader(strings.Repeat("!", int(payload.Size)))), ContentLength: payload.Size, Request: req}, nil
					}
					return http.DefaultTransport.RoundTrip(req)
				})}
			}
			pushed, err := s.Push(ctx, plan)
			if scenario == "missing" || scenario == "corrupt" {
				if err == nil {
					t.Fatal("push accepted unavailable/corrupt payload")
				}
				if _, ok := target.objects["release|manifests|v1"]; ok {
					t.Fatal("failed push published completion tag")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if pushed.Descriptor.Digest != created.Descriptor.Digest {
				t.Fatal("push changed bundle snapshot")
			}
			if r.headTags["stable"] != resolutions {
				t.Fatal("push refreshed mutable source tag")
			}
			if scenario == "reuse" {
				if r.blobReads != 0 || pushed.Stats.BlobBytesDownloaded != 0 {
					t.Fatalf("reused payload downloaded: reads=%d stats=%+v", r.blobReads, pushed.Stats)
				}
			} else if r.blobReads == 0 || pushed.Stats.BlobBytesDownloaded == 0 {
				t.Fatalf("cross-registry push did not stream payload: reads=%d stats=%+v", r.blobReads, pushed.Stats)
			}
		})
	}
}

func TestCreateLocalRetainsSharedInlinePayloadInEitherTraversalOrder(t *testing.T) {
	for _, inlineFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(inlineFirst), func(t *testing.T) {
			r, repo, s, _ := monitoredRegistry(t)
			payloadBytes := []byte("shared inline payload")
			payload := put(t, r.store, "application/example", payloadBytes)
			cfg := put(t, r.store, v1.MediaTypeEmptyJSON, []byte("{}"))
			makeRoot := func(inline bool) v1.Descriptor {
				d := payload
				if inline {
					d.Data = payloadBytes
				}
				raw, _ := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{d}})
				return put(t, r.store, v1.MediaTypeImageManifest, raw)
			}
			r.tags["inline"], r.tags["plain"] = makeRoot(true), makeRoot(false)
			first, second := "plain", "inline"
			if inlineFirst {
				first, second = second, first
			}
			recipe := config.Recipe{Include: []config.Selection{dependencySelection(repo + ":" + first), dependencySelection(repo + ":" + second)}}
			path := filepath.Join(t.TempDir(), "capture")
			if _, err := s.CreateLocal(ctx, recipe, path, CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if r.blobReads != 0 {
				t.Fatal("capture fetched shared inline bytes remotely")
			}
			// The shared bytes are sufficient even after their source becomes unavailable.
			delete(r.store.descriptors, payload.Digest.String())
			if _, err := s.Save(ctx, path, filepath.Join(t.TempDir(), "offline.tar")); err != nil {
				t.Fatal(err)
			}
			store, root, err := readLayoutRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(path, "blobs", payload.Digest.Algorithm().String(), payload.Digest.Encoded())); err != nil {
				t.Fatal(err)
			}
			snapshot, err := bundle.InspectSnapshotGraph(ctx, store, root)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(path, captureFile))
			if err != nil {
				t.Fatal(err)
			}
			var state CaptureState
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			if err := ValidateCaptureState(ctx, store, snapshot, state); err == nil {
				t.Fatal("inline edge waived missing non-inline payload locator")
			}
		})
	}
}
