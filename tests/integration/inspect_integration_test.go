package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
)

func TestPublicInspectCapturedBundleWithoutSources(t *testing.T) {
	ctx := context.Background()
	source, holding := httptest.NewServer(newSDKRegistry()), httptest.NewServer(newSDKRegistry())
	defer source.Close()
	defer holding.Close()
	sourceHost, holdingHost := strings.TrimPrefix(source.URL, "http://"), strings.TrimPrefix(holding.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{sourceHost, holdingHost}})
	if err != nil {
		t.Fatal(err)
	}
	subject := publishDependencyArtifact(t, client, sourceHost, dependencyArtifact(t, "application", false), "app")
	image := publishDependencyArtifact(t, client, sourceHost, dependencyArtifact(t, "image", true), "runtime")
	prepared, err := bale.PrepareDependencyDeclaration(ctx, subject, bale.DependencyDeclaration{Dependencies: []bale.DependencyReference{{Ref: image.Repository + ":runtime"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Attach(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	reference := holdingHost + "/bundles:inspect"
	created, err := client.Create(ctx, bale.Recipe{Include: []bale.Selection{{Ref: subject.Repository + ":app", Referrers: bale.ReferrerPolicy{Include: "none"}}}}, reference, bale.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	// A fresh client knows only the holding registry. Inspection must not resolve
	// dependency selectors or rediscover referrers in the original repositories.
	var rootResolutions atomic.Int32
	reader, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{holdingHost}, HTTPClient: &http.Client{Transport: inspectRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			t.Errorf("inspection wrote registry: %s %s", request.Method, request.URL)
			return nil, os.ErrPermission
		}
		if strings.Contains(request.URL.Path, "/referrers/") || request.URL.Host != holdingHost {
			t.Errorf("inspection discovered uncaptured metadata: %s", request.URL)
			return nil, os.ErrPermission
		}
		if request.URL.Path == "/v2/bundles/manifests/inspect" {
			rootResolutions.Add(1)
		}
		return http.DefaultTransport.RoundTrip(request)
	})}})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := reader.Inspect(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	if rootResolutions.Load() != 1 {
		t.Fatalf("root resolved %d times", rootResolutions.Load())
	}
	if !reflect.DeepEqual(remote.Metadata, created.Metadata) {
		t.Fatal("inspection changed captured metadata")
	}
	if remote.Descriptor.Digest != created.Descriptor.Digest || len(remote.Declarations) != 1 {
		t.Fatalf("unexpected inspection: %+v", remote)
	}
	declaration := remote.Declarations[0]
	if declaration.Repository != subject.Repository || declaration.Subject.Digest != subject.Descriptor.Digest || declaration.Descriptor.Digest != prepared.Descriptor.Digest || len(declaration.Declaration.Dependencies) != 1 || declaration.Declaration.Dependencies[0].Ref != image.Repository+":runtime" {
		t.Fatalf("unexpected declaration: %+v", declaration)
	}
	layout := filepath.Join(t.TempDir(), "bundle")
	if _, err := reader.Pull(ctx, reference, layout, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	holding.Close()
	offline, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := inspectLayoutFiles(t, layout)
	local, err := offline.InspectLayout(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(remote, local) {
		t.Fatal("remote and layout inspection differ")
	}
	store := oci.NewStorageFromFS(os.DirFS(layout))
	owned, err := offline.InspectRoot(ctx, bale.Root{Descriptor: local.Descriptor, Store: store})
	if err != nil || !reflect.DeepEqual(local, owned) {
		t.Fatalf("caller-owned inspection differs: %v", err)
	}
	if !reflect.DeepEqual(before, inspectLayoutFiles(t, layout)) {
		t.Fatal("inspection modified layout")
	}
	raw, err := json.Marshal(owned)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["descriptor"] == nil || fields["metadata"] == nil || fields["declarations"] == nil {
		t.Fatalf("unexpected JSON fields: %s", raw)
	}
	// A complete index is insufficient: every captured child must be present and
	// match its digest even though inspection emits only metadata.
	blob := filepath.Join(layout, "blobs", image.Descriptor.Digest.Algorithm().String(), image.Descriptor.Digest.Encoded())
	original, err := os.ReadFile(blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blob); err != nil {
		t.Fatal(err)
	}
	if _, err := offline.InspectLayout(ctx, layout); !errors.Is(err, bale.ErrNotFound) {
		t.Fatalf("missing child: %v", err)
	}
	if err := os.WriteFile(blob, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := offline.InspectLayout(ctx, layout); !errors.Is(err, bale.ErrIntegrity) {
		t.Fatalf("corrupt child: %v", err)
	}
	if err := os.WriteFile(blob, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := offline.InspectLayout(ctx, layout); err != nil {
		t.Fatal(err)
	}
	t.Run("invalid capture marker", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("creating symlinks on Windows can require extra privileges")
		}
		const marker = "bale-capture.json"
		// The marker loops within the fixture; no path outside it is needed.
		if err := os.Symlink(marker, filepath.Join(layout, marker)); err != nil {
			if errors.Is(err, os.ErrPermission) {
				t.Skipf("creating symlinks is not permitted: %v", err)
			}
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(filepath.Join(layout, marker)) })
		_, err := offline.InspectLayout(ctx, layout)
		if !errors.Is(err, bale.ErrIntegrity) || errors.Is(err, bale.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid capture marker: %v", err)
		}
		var cause *os.PathError
		if !errors.As(err, &cause) || cause.Path != marker {
			t.Fatalf("want original marker open error, got %v", err)
		}
	})
	t.Run("unreadable capture marker target", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("fixture requires unprivileged POSIX file permissions")
		}
		target := filepath.Join(layout, "marker-target.json")
		if err := os.WriteFile(target, []byte("{}"), 0); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Base(target), filepath.Join(layout, "bale-capture.json")); err != nil {
			if errors.Is(err, os.ErrPermission) {
				t.Skipf("creating symlinks is not permitted: %v", err)
			}
			t.Fatal(err)
		}
		_, err := offline.InspectLayout(ctx, layout)
		if !errors.Is(err, os.ErrPermission) || errors.Is(err, bale.ErrIntegrity) {
			t.Fatalf("expected ordinary permission denial, got %v", err)
		}
	})
}

func inspectLayoutFiles(t *testing.T, path string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(path, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[path] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

type inspectFailedStore struct{ err error }

func (s inspectFailedStore) Fetch(context.Context, v1.Descriptor) (io.ReadCloser, error) {
	return nil, s.err
}
func (s inspectFailedStore) Exists(context.Context, v1.Descriptor) (bool, error) { return false, s.err }

func TestPublicInspectFailuresAndReadOnlyPaths(t *testing.T) {
	ctx := context.Background()
	client, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "missing", "bundle")
	if _, err := client.InspectLayout(ctx, path); !errors.Is(err, bale.ErrNotFound) {
		t.Fatalf("missing layout: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("inspection created missing directory: %v", err)
	}
	if _, err := client.InspectLayout(ctx, ""); !errors.Is(err, bale.ErrInvalid) {
		t.Fatalf("empty layout: %v", err)
	}
	if _, err := client.InspectRoot(ctx, bale.Root{}); !errors.Is(err, bale.ErrInvalid) {
		t.Fatalf("missing root: %v", err)
	}
	root := dependencyArtifact(t, "nonbundle", false)
	if _, err := client.InspectRoot(ctx, root); !errors.Is(err, bale.ErrUnsupported) {
		t.Fatalf("nonbundle: %v", err)
	}
	root.Store = inspectFailedStore{os.ErrPermission}
	if _, err := client.InspectRoot(ctx, root); !errors.Is(err, os.ErrPermission) || errors.Is(err, bale.ErrIntegrity) {
		t.Fatalf("permission: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := client.InspectRoot(canceled, root); !errors.Is(err, context.Canceled) || errors.Is(err, bale.ErrIntegrity) {
		t.Fatalf("cancellation: %v", err)
	}
	root.Store = inspectFailedStore{content.ErrMismatchedDigest}
	if _, err := client.InspectRoot(ctx, root); !errors.Is(err, bale.ErrIntegrity) {
		t.Fatalf("digest mismatch: %v", err)
	}
}

type inspectRoundTripper func(*http.Request) (*http.Response, error)

func (f inspectRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
