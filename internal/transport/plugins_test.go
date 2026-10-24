package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
	"github.com/strio-dev/bale/internal/pluginoci"
	"github.com/strio-dev/bale/internal/wasmtest"
	"oras.land/oras-go/v2/content"
)

func capturePluginFixture(t *testing.T, r *readRegistry) v1.Descriptor {
	t.Helper()
	path := filepath.Join(t.TempDir(), "discovery-0.0.0.tgz")
	if err := os.WriteFile(path, wasmtest.Package([]byte{'\x00', 'a', 's', 'm', 1, 0, 0, 0}), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := pluginoci.Pack(ctx, r.store, path)
	if err != nil {
		t.Fatal(err)
	}
	r.tags["plugin"] = d
	return d
}

func TestConfiguredPluginCaptureProfilesAndLockedClosure(t *testing.T) {
	r, repo, s := testRegistry(t)
	s.client.Client = &http.Client{Transport: deliveryRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			return nil, fmt.Errorf("unexpected source write: %s %s", request.Method, request.URL)
		}
		return http.DefaultTransport.RoundTrip(request)
	})}

	subject := object(t, r.store, "selected subject")
	imageA := object(t, r.store, "image A")
	imageB := object(t, r.store, "image B")
	r.tags["chart"] = subject
	r.tags["alias"] = subject
	r.tags["a"] = imageA
	r.tags["b"] = imageB
	module := capturePluginFixture(t, r)
	old := dependencyDeclaration(t, r, subject, "existing", dependency.Reference{Ref: repo + ":a"})
	calls := 0
	s.pluginRunner = func(ctx context.Context, request PluginRequest) (dependency.Declaration, error) {
		calls++
		if raw, err := os.ReadFile(request.ModulePath); err != nil || len(raw) != 8 {
			t.Fatalf("module was not materialized: %v", err)
		}
		var input struct {
			Ref string `json:"ref"`
		}
		if err := json.Unmarshal(request.Input, &input); err != nil {
			t.Fatal(err)
		}
		return dependency.Declaration{Context: input.Ref, Dependencies: []dependency.Reference{{Ref: input.Ref}}}, nil
	}
	first := dependencySelection("")
	first.Repository = repo
	tags := []string{"chart", "alias"}
	first.Tags = &tags
	first.Plugin = &config.Plugin{Ref: repo + ":plugin", Input: json.RawMessage(`{"ref":"` + repo + `:a"}`)}
	second := dependencySelection(repo + ":chart")
	second.Plugin = &config.Plugin{Ref: repo + ":plugin", Input: json.RawMessage(`{"ref":"` + repo + `:b"}`)}
	recipe := config.Recipe{Include: []config.Selection{first, second}}
	g, snapshot, err := s.capture(ctx, recipe)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(snapshot.Metadata.Plugins) != 2 || len(snapshot.Metadata.Roots) != 3 {
		t.Fatalf("profiles/aliases lost: calls=%d metadata=%+v", calls, snapshot.Metadata)
	}
	if r.headTags["plugin"] != 1 || r.headTags["chart"] != 1 {
		t.Fatalf("tags were not pinned once: %+v", r.headTags)
	}
	if !capturedOrigin(snapshot.Metadata, module) || !capturedOrigin(snapshot.Metadata, imageA) || !capturedOrigin(snapshot.Metadata, imageB) {
		t.Fatal("missing plugin or dependency origins")
	}
	if len(r.refs[subject.Digest.String()]) != 1 || r.refs[subject.Digest.String()][0].Digest != old.Digest {
		t.Fatal("source inventory changed")
	}
	if snapshot.Metadata.Plugins[0].Declaration.Digest == snapshot.Metadata.Plugins[1].Declaration.Digest {
		t.Fatal("different profiles conflated")
	}
	for _, p := range snapshot.Metadata.Plugins {
		if exists, err := r.store.Exists(ctx, p.Declaration); err != nil || exists {
			t.Fatal("generated declaration was written to source")
		}
	}
	// Materialize entirely from the captured immutable graph, then inspect without
	// running the producer or consulting its source inventory.
	target := newMetadataStore()
	if err := copyGraph(ctx, g, target, snapshot.Descriptor, &Stats{}, nil); err != nil {
		t.Fatal(err)
	}
	s.pluginRunner = func(context.Context, PluginRequest) (dependency.Declaration, error) {
		t.Fatal("locked graph executed plugin")
		return dependency.Declaration{}, nil
	}
	if _, err := bundle.LoadSnapshotGraph(ctx, target, snapshot.Descriptor); err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredPluginNeedsRunnerBeforeNetwork(t *testing.T) {
	s, err := NewService(Options{})
	if err != nil {
		t.Fatal(err)
	}
	input := dependencySelection("unreachable.invalid/chart:tag")
	input.Plugin = &config.Plugin{Ref: "unreachable.invalid/plugin:tag", Input: json.RawMessage(`{}`)}
	if _, _, err = s.capture(ctx, config.Recipe{Include: []config.Selection{input}}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "runner") {
		t.Fatalf("missing preflight: %v", err)
	}
}

func TestConfiguredPluginBindingAndRunnerFailure(t *testing.T) {
	for _, mode := range []string{"binding", "collision", "missing", "runner failure", "invalid declaration"} {
		t.Run(mode, func(t *testing.T) {
			r, repo, s := testRegistry(t)
			subject := object(t, r.store, "opaque chart bytes")
			r.tags["chart"] = subject
			capturePluginFixture(t, r)
			var manifest v1.Manifest
			raw, err := content.FetchAll(ctx, r.store, subject)
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			input := dependencySelection(repo + ":chart")
			input.Plugin = &config.Plugin{Ref: repo + ":plugin", Input: json.RawMessage(`{}`), Blobs: map[string]config.PluginBlob{"chart": {MediaType: manifest.Layers[0].MediaType}}}
			if mode == "collision" {
				input.Plugin.Input = json.RawMessage(`{"chart":"collision"}`)
			}
			if mode == "missing" {
				input.Plugin.Blobs["chart"] = config.PluginBlob{MediaType: "missing/type"}
			}
			calls := 0
			s.pluginRunner = func(_ context.Context, request PluginRequest) (dependency.Declaration, error) {
				calls++
				var bound map[string]string
				if err := json.Unmarshal(request.Input, &bound); err != nil {
					t.Fatal(err)
				}
				decoded, err := base64.StdEncoding.DecodeString(bound["chart"])
				if err != nil || string(decoded) != "opaque chart bytes" {
					t.Fatalf("wrong pinned layer: %q %v", decoded, err)
				}
				if mode == "runner failure" {
					return dependency.Declaration{}, errors.New("producer failed")
				}
				if mode == "invalid declaration" {
					return dependency.Declaration{SchemaVersion: 99}, nil
				}
				return dependency.Declaration{Dependencies: []dependency.Reference{}}, nil
			}
			_, snapshot, err := s.capture(ctx, config.Recipe{Include: []config.Selection{input}})
			if mode == "binding" {
				if err != nil {
					t.Fatal(err)
				}
				if len(snapshot.Metadata.Plugins) != 1 {
					t.Fatal("missing plugin record")
				}
			} else if err == nil {
				t.Fatal("accepted failed producer/binding")
			}
			if (mode == "collision" || mode == "missing") && calls != 0 {
				t.Fatal("ran producer with invalid bindings")
			}
			if len(r.refs[subject.Digest.String()]) != 0 {
				t.Fatal("mutated source on capture")
			}
		})
	}
}

func TestConfiguredPluginLatestPermissionAndLockedPull(t *testing.T) {
	r, repo, s := testRegistry(t)
	subject := object(t, r.store, "subject")
	r.tags["chart"] = subject
	capturePluginFixture(t, r)
	calls := 0
	s.pluginRunner = func(context.Context, PluginRequest) (dependency.Declaration, error) {
		calls++
		return dependency.Declaration{}, nil
	}
	input := dependencySelection(repo + ":chart")
	input.Plugin = &config.Plugin{Ref: repo + ":plugin", Input: json.RawMessage(`{}`)}
	g, snapshot, err := s.capture(ctx, config.Recipe{Include: []config.Selection{input}})
	if err != nil {
		t.Fatal(err)
	}
	if err := copyGraph(ctx, g, r.store, snapshot.Descriptor, &Stats{}, nil); err != nil {
		t.Fatal(err)
	}
	r.tags["bundle"] = snapshot.Descriptor
	calls = 0
	before := r.headTags["plugin"]
	deniedPath := filepath.Join(t.TempDir(), "denied")
	if _, err := s.Pull(ctx, repo+":bundle", deniedPath, PullLatest); !errors.Is(err, ErrInvalid) {
		t.Fatalf("latest without permission: %v", err)
	}
	if calls != 0 || r.headTags["plugin"] != before {
		t.Fatal("unapproved latest resolved or executed a plugin")
	}
	if _, err := os.Lstat(deniedPath); !os.IsNotExist(err) {
		t.Fatalf("denied latest created layout: %v", err)
	}
	lockedPath := filepath.Join(t.TempDir(), "locked")
	if _, err := s.Pull(ctx, repo+":bundle", lockedPath, PullLocked); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || r.headTags["plugin"] != before {
		t.Fatal("locked pull resolved or executed plugin")
	}
	refreshed := filepath.Join(t.TempDir(), "latest")
	if _, err := s.PullWithOptions(ctx, repo+":bundle", refreshed, PullLatest, PullOptions{AllowPlugins: true}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || r.headTags["plugin"] != before+1 {
		t.Fatal("approved latest did not refresh plugin once")
	}
}

func TestPluginOverlayKeepsRemotePayloadCaptureLazy(t *testing.T) {
	r, repo, s := testRegistry(t)
	subject := object(t, r.store, "large opaque payload")
	g := newGraph(s)
	g.local = newMetadataStore()
	if err := g.visit(ctx, repo, subject, false, true); err != nil {
		t.Fatal(err)
	}
	if r.blobReads != 0 || g.stats.BlobBytesDownloaded != 0 {
		t.Fatalf("metadata overlay downloaded opaque payloads: reads=%d bytes=%d", r.blobReads, g.stats.BlobBytesDownloaded)
	}
}

func TestConfiguredPluginCreateDryRunAndFailureDoNotWrite(t *testing.T) {
	r, repo, s := testRegistry(t)
	subject := object(t, r.store, "subject")
	r.tags["chart"] = subject
	capturePluginFixture(t, r)
	s.client.Client = &http.Client{Transport: deliveryRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			return nil, fmt.Errorf("unexpected registry write: %s", request.Method)
		}
		return http.DefaultTransport.RoundTrip(request)
	})}
	input := dependencySelection(repo + ":chart")
	input.Plugin = &config.Plugin{Ref: repo + ":plugin", Input: json.RawMessage(`{}`)}
	recipe := config.Recipe{Include: []config.Selection{input}}
	calls := 0
	s.pluginRunner = func(context.Context, PluginRequest) (dependency.Declaration, error) {
		calls++
		return dependency.Declaration{}, nil
	}
	result, err := s.Create(ctx, recipe, repo+":bundle", CreateOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.Published || !result.DryRun || len(result.Metadata.Plugins) != 1 {
		t.Fatalf("wrong dry run: %+v calls=%d", result, calls)
	}
	if _, exists := r.tags["bundle"]; exists {
		t.Fatal("dry run published completion")
	}
	s.pluginRunner = func(context.Context, PluginRequest) (dependency.Declaration, error) {
		return dependency.Declaration{}, errors.New("producer failure")
	}
	if _, err := s.Create(ctx, recipe, repo+":bundle", CreateOptions{}); err == nil {
		t.Fatal("accepted producer failure")
	}
	if _, exists := r.tags["bundle"]; exists {
		t.Fatal("failed producer published completion")
	}
}

func TestConfiguredPluginInlineSubjectBinding(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt=%t", corrupt), func(t *testing.T) {
			r, repo, s := testRegistry(t)
			capturePluginFixture(t, r)
			chart := []byte("inline chart bytes")
			layer := content.NewDescriptorFromBytes("application/example.chart", chart)
			layer.Data = append([]byte(nil), chart...)
			if corrupt {
				layer.Data[0] ^= 1
			}
			cfg := put(t, r.store, v1.MediaTypeEmptyJSON, []byte("{}"))
			raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{layer}})
			if err != nil {
				t.Fatal(err)
			}
			subject := put(t, r.store, v1.MediaTypeImageManifest, raw)
			r.tags["chart"] = subject
			if exists, err := r.store.Exists(ctx, layer); err != nil || exists {
				t.Fatalf("inline-only fixture has backing blob: %t %v", exists, err)
			}
			boundReads := 0
			s.client.Client = &http.Client{Transport: deliveryRoundTripper(func(request *http.Request) (*http.Response, error) {
				if strings.HasSuffix(request.URL.Path, "/blobs/"+layer.Digest.String()) {
					boundReads++
					return nil, fmt.Errorf("inline chart unexpectedly fetched from source")
				}
				return http.DefaultTransport.RoundTrip(request)
			})}
			calls := 0
			s.pluginRunner = func(_ context.Context, request PluginRequest) (dependency.Declaration, error) {
				calls++
				var input map[string][]byte
				if err := json.Unmarshal(request.Input, &input); err != nil {
					t.Fatal(err)
				}
				if string(input["chart"]) != string(chart) {
					t.Fatalf("wrong inline binding: %q", input["chart"])
				}
				return dependency.Declaration{}, nil
			}
			input := dependencySelection(repo + ":chart")
			input.Plugin = &config.Plugin{Ref: repo + ":plugin", Input: json.RawMessage(`{}`), Blobs: map[string]config.PluginBlob{"chart": {MediaType: layer.MediaType}}}
			_, snapshot, err := s.capture(ctx, config.Recipe{Include: []config.Selection{input}})
			if corrupt {
				if err == nil || calls != 0 {
					t.Fatalf("corrupt inline data reached runner: calls=%d err=%v", calls, err)
				}
			} else if err != nil || calls != 1 || len(snapshot.Metadata.Plugins) != 1 {
				t.Fatalf("inline binding capture failed: calls=%d err=%v", calls, err)
			}
			if boundReads != 0 {
				t.Fatalf("inline binding made %d source blob requests", boundReads)
			}
		})
	}
}
