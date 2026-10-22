package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func TestCreateLayoutRetainsNestedPluginAndDependencyClosure(t *testing.T) {
	r, repo, s, writes := monitoredRegistry(t)
	subject := object(t, r.store, "subject")
	image := object(t, r.store, "dependency")
	child := snapshotFor(t, r.store, []string{repo}, []v1.Descriptor{object(t, r.store, "nested")})
	r.tags["subject"], r.tags["image"], r.tags["child"] = subject, image, child.Descriptor
	module := capturePluginFixture(t, r)
	calls := 0
	s.pluginRunner = func(context.Context, PluginRequest) (dependency.Declaration, error) {
		calls++
		return dependency.Declaration{Dependencies: []dependency.Reference{{Ref: repo + ":image"}}}, nil
	}
	selected := dependencySelection(repo + ":subject")
	selected.Plugin = &config.Plugin{Ref: repo + ":plugin", Input: json.RawMessage(`{}`)}
	recipe := config.Recipe{Include: []config.Selection{selected, dependencySelection(repo + ":child")}}
	_, expected, err := s.capture(ctx, recipe)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bundle")
	result, err := s.CreateLayout(ctx, recipe, path, CreateOptions{ExportDelivery: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Published || result.LayoutPath != path || result.Delivery == nil || result.Descriptor.Digest != expected.Descriptor.Digest {
		t.Fatalf("capture differs: %+v", result)
	}
	if result.Stats.BlobBytesUploaded != 0 || result.Stats.ManifestBytesUploaded != 0 || result.Stats.BlobBytesDownloaded == 0 {
		t.Fatalf("local writes counted as registry uploads or source reads missing: %+v", result.Stats)
	}
	store, snapshot, err := openLayout(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Descriptor.Digest != expected.Descriptor.Digest || len(snapshot.Metadata.Plugins) != 1 || len(snapshot.Metadata.Dependencies) == 0 || calls != 2 {
		t.Fatalf("captured metadata lost: %+v; calls=%d", snapshot.Metadata, calls)
	}
	for _, d := range []v1.Descriptor{subject, image, module, child.Descriptor} {
		if _, err := content.FetchAll(ctx, store, d); err != nil {
			t.Fatalf("missing %s: %v", d.Digest, err)
		}
	}
	if writes.Load() != 0 {
		t.Fatal("local capture published to source registry")
	}
}

func TestCreateLayoutDryRunAndDestinationPreflight(t *testing.T) {
	r, repo, s, writes := monitoredRegistry(t)
	r.tags["stable"] = object(t, r.store, "payload")
	recipe := config.Recipe{Include: []config.Selection{dependencySelection(repo + ":stable")}}
	parent := filepath.Join(t.TempDir(), "absent-parent")
	path := filepath.Join(parent, "bundle")
	result, err := s.CreateLayout(ctx, recipe, path, CreateOptions{DryRun: true, ExportDelivery: true})
	if err != nil || !result.DryRun || result.LayoutPath != "" || result.Published || result.Delivery == nil {
		t.Fatalf("dry run: %+v %v", result, err)
	}
	if _, err := os.Lstat(parent); !os.IsNotExist(err) {
		t.Fatalf("dry run created filesystem state: %v", err)
	}
	if writes.Load() != 0 {
		t.Fatal("dry run wrote source")
	}
	for _, kind := range []string{"directory", "symlink", "file"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "existing")
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(path, 0755)
			case "symlink":
				err = os.Symlink("missing", path)
			case "file":
				err = os.WriteFile(path, []byte("keep"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := r.headTags["stable"]
			for _, dry := range []bool{false, true} {
				if _, err := s.CreateLayout(ctx, recipe, path, CreateOptions{DryRun: dry}); !errors.Is(err, ErrConflict) {
					t.Fatalf("existing target accepted: %v", err)
				}
			}
			if before != r.headTags["stable"] {
				t.Fatal("preflight read source")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("existing entry changed", err)
			}
		})
	}
}

type layoutFetchFunc func(context.Context, v1.Descriptor) (io.ReadCloser, error)

func (f layoutFetchFunc) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	return f(ctx, d)
}

func TestWriteLayoutFailureCancellationAndPublicationRace(t *testing.T) {
	for _, scenario := range []string{"source failure", "cancel", "destination race", "invalid root"} {
		t.Run(scenario, func(t *testing.T) {
			src := memory.New()
			root := object(t, src, "payload")
			if scenario != "invalid root" {
				root = snapshotFor(t, src, []string{"source.example/app"}, []v1.Descriptor{root}).Descriptor
			}
			parent := t.TempDir()
			path := filepath.Join(parent, "bundle")
			testCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			first := true
			failure := errors.New("injected source failure")
			fetcher := layoutFetchFunc(func(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
				if first {
					first = false
					switch scenario {
					case "source failure":
						return nil, failure
					case "cancel":
						cancel()
						return nil, context.Canceled
					case "destination race":
						if err := os.Mkdir(path, 0755); err != nil {
							t.Fatal(err)
						}
					}
				}
				return src.Fetch(ctx, d)
			})
			err := writeLayout(testCtx, fetcher, root, path, &Stats{})
			if err == nil {
				t.Fatal("failed layout published")
			}
			if scenario == "source failure" && !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if scenario == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if scenario == "destination race" && !errors.Is(err, ErrConflict) {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "destination race" {
				if len(entries) != 1 || entries[0].Name() != "bundle" {
					t.Fatalf("race cleanup: %v", entries)
				}
				contents, err := os.ReadDir(path)
				if err != nil || len(contents) != 0 {
					t.Fatalf("existing directory replaced: %v %v", contents, err)
				}
			} else if len(entries) != 0 {
				t.Fatalf("partial state left: %v", entries)
			}
		})
	}
}

func TestLayoutRejectsHashValidInvalidNestedMetadata(t *testing.T) {
	store := memory.New()
	child := snapshotFor(t, store, []string{"source.example/member"}, []v1.Descriptor{object(t, store, "member")})
	raw, err := content.FetchAll(ctx, store, child.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	index.Annotations[bundle.MetadataDigestKey] = digest.FromString("unselected metadata").String()
	raw, err = json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	invalidChild := put(t, store, v1.MediaTypeImageIndex, raw)
	invalidChild.ArtifactType = bundle.BundleArtifactType
	outer := snapshotFor(t, store, []string{"source.example/child"}, []v1.Descriptor{invalidChild})
	// Every digest and the outer metadata are valid. The nested native boundary
	// must still be interpreted rather than accepted as an opaque OCI index.
	if err := bundle.VerifyClosure(ctx, store, outer.Descriptor); err != nil {
		t.Fatalf("fixture closure: %v", err)
	}
	parent := t.TempDir()
	path := filepath.Join(parent, "rejected")
	if err := writeLayout(ctx, store, outer.Descriptor, path, &Stats{}); err == nil {
		t.Fatal("published invalid nested bundle")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed publication left partial state: %v %v", entries, err)
	}
	// A hand-crafted existing layout must be rejected by readers too, including
	// the archive save path that opens layouts through this shared helper.
	forged := layoutFor(t, store, outer.Descriptor)
	if _, _, err := openLayout(ctx, forged); err == nil {
		t.Fatal("opened invalid nested bundle")
	}
}
