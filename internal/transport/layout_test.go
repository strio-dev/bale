package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/deliveryfile"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func TestStandaloneLayoutHeaders(t *testing.T) {
	src := memory.New()
	root := snapshotFor(t, src, []string{"example.test/bundle"}, []v1.Descriptor{object(t, src, "payload")}).Descriptor
	svc := &Service{}
	for _, tc := range []struct {
		name  string
		alter func(*v1.Index)
	}{
		{"optional metadata", func(index *v1.Index) {
			index.MediaType = ""
			index.Annotations = map[string]string{v1.AnnotationTitle: "bundle"}
		}},
		{"wrong schema", func(index *v1.Index) { index.SchemaVersion = 1 }},
		{"empty index", func(index *v1.Index) { index.Manifests = []v1.Descriptor{} }},
		{"multiple roots", func(index *v1.Index) { index.Manifests = append(index.Manifests, root) }},
		{"wrong media type", func(index *v1.Index) { index.MediaType = v1.MediaTypeImageManifest }},
		{"subject", func(index *v1.Index) { index.Subject = &root }},
		{"artifact type", func(index *v1.Index) { index.ArtifactType = "application/example" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := layoutFor(t, src, root)
			index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{root}}
			tc.alter(&index)
			raw, err := json.Marshal(index)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(layout, "index.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}

			saved, saveErr := svc.SaveTo(t.Context(), layout, io.Discard)
			inspected, inspectErr := svc.InspectLayout(t.Context(), layout)
			if tc.name == "optional metadata" {
				if saveErr != nil || !content.Equal(saved.Descriptor, root) {
					t.Fatalf("save: descriptor=%+v error=%v", saved.Descriptor, saveErr)
				}
				if inspectErr != nil || !content.Equal(inspected.Descriptor, root) {
					t.Fatalf("inspect: descriptor=%+v error=%v", inspected.Descriptor, inspectErr)
				}
			} else {
				checkStandaloneHeaderError(t, "index.json", saveErr, inspectErr)
			}
		})
	}
}

func TestStandaloneLayoutHeaderFileLimits(t *testing.T) {
	const layoutHeaderLimit = 8 << 20 // Same per-header limit as the OCI archive reader.
	src := memory.New()
	root := snapshotFor(t, src, []string{"example.test/bundle"}, []v1.Descriptor{object(t, src, "payload")}).Descriptor
	svc := &Service{}
	for _, header := range []string{"oci-layout", "index.json"} {
		for _, kind := range []string{"oversized", "directory", "symlink"} {
			t.Run(header+"/"+kind, func(t *testing.T) {
				layout := layoutFor(t, src, root)
				filename := filepath.Join(layout, header)
				switch kind {
				case "oversized":
					// Valid JSON with trailing whitespace isolates the size check.
					raw, err := os.ReadFile(filename)
					if err != nil {
						t.Fatal(err)
					}
					raw = append(raw, strings.Repeat(" ", layoutHeaderLimit+1-len(raw))...)
					if err := os.WriteFile(filename, raw, 0600); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Remove(filename); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(filename, 0700); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					target := filename + ".target"
					if err := os.Rename(filename, target); err != nil {
						t.Fatal(err)
					}
					symlinkLayoutFixture(t, filepath.Base(target), filename)
				}

				saved, saveErr := svc.SaveTo(t.Context(), layout, io.Discard)
				inspected, inspectErr := svc.InspectLayout(t.Context(), layout)
				if kind == "symlink" {
					if saveErr != nil || !content.Equal(saved.Descriptor, root) {
						t.Fatalf("save via contained header link: descriptor=%+v error=%v", saved.Descriptor, saveErr)
					}
					if inspectErr != nil || !content.Equal(inspected.Descriptor, root) {
						t.Fatalf("inspect via contained header link: descriptor=%+v error=%v", inspected.Descriptor, inspectErr)
					}
					return
				}
				checkStandaloneHeaderError(t, header, saveErr, inspectErr)
			})
		}
	}
}

func symlinkLayoutFixture(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		if errors.Is(err, fs.ErrPermission) || symlinkPrivilegeNotHeld(err) {
			t.Skipf("creating symlinks is not permitted: %v", err)
		}
		t.Fatal(err)
	}
}

func checkStandaloneHeaderError(t *testing.T, member string, saveErr, inspectErr error) {
	t.Helper()
	if saveErr == nil || !strings.Contains(saveErr.Error(), member) {
		t.Fatalf("save: want invalid %s, got %v", member, saveErr)
	}
	if !errors.Is(inspectErr, ErrIntegrity) || !strings.Contains(inspectErr.Error(), member) {
		t.Fatalf("inspect: want integrity error for %s, got %v", member, inspectErr)
	}
}

func openLayoutTestRoot(t *testing.T, path string) (content.ReadOnlyStorage, v1.Descriptor, error) {
	t.Helper()
	layout, root, err := readLayoutRoot(path)
	if err != nil {
		return nil, v1.Descriptor{}, err
	}
	t.Cleanup(layout.Close)
	return layout.store, root, nil
}

func openLayout(t *testing.T, ctx context.Context, path string) (content.ReadOnlyStorage, bundle.Snapshot, error) {
	t.Helper()
	store, root, err := openLayoutTestRoot(t, path)
	if err != nil {
		return nil, bundle.Snapshot{}, err
	}
	snapshot, err := bundle.LoadSnapshotGraph(ctx, store, root)
	return store, snapshot, err
}

func TestExternalLayoutFilePolicy(t *testing.T) {
	src := memory.New()
	root := snapshotFor(t, src, []string{"example.test/bundle"}, []v1.Descriptor{object(t, src, "payload")}).Descriptor

	t.Run("regular files and ownership", func(t *testing.T) {
		path := layoutFor(t, src, root)
		layout, got, err := readLayoutRoot(path)
		if err != nil {
			t.Fatal(err)
		}
		defer layout.Close()
		if !content.Equal(got, root) {
			t.Fatalf("got descriptor %v; want %v", got, root)
		}
		if _, err := content.FetchAll(t.Context(), layout.store, root); err != nil {
			t.Fatal(err)
		}
		if exists, err := layout.store.Exists(t.Context(), root); !exists || err != nil {
			t.Fatalf("existing blob: exists=%v err=%v", exists, err)
		}
		absent := content.NewDescriptorFromBytes(root.MediaType, []byte("absent"))
		if exists, err := layout.store.Exists(t.Context(), absent); exists || err != nil {
			t.Fatalf("absent blob: exists=%v err=%v", exists, err)
		}
		files := layout.files
		for _, name := range []string{".", "/index.json", "../index.json", "blobs//leaf"} {
			if f, err := files.Open(name); !errors.Is(err, fs.ErrInvalid) {
				if f != nil {
					_ = f.Close()
				}
				t.Errorf("invalid member %q: %v", name, err)
			}
		}
		layout.Close()
		if _, err := content.FetchAll(t.Context(), layout.store, root); err == nil {
			t.Fatal("store retained access after its owner closed")
		}
	})

	t.Run("empty root path", func(t *testing.T) {
		layout, _, err := readLayoutRoot("")
		if layout != nil {
			layout.Close()
		}
		if layout != nil || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("want empty layout root rejected, got %v", err)
		}
	})

	t.Run("explicit root alias", func(t *testing.T) {
		path := layoutFor(t, src, root)
		alias := filepath.Join(t.TempDir(), "alias")
		symlinkLayoutFixture(t, path, alias)
		result, err := (&Service{}).InspectLayout(t.Context(), alias)
		if err != nil || !content.Equal(result.Descriptor, root) {
			t.Fatalf("inspect explicit layout root alias: %+v %v", result, err)
		}
	})

	for _, kind := range []string{"directory leaf", "symlink leaf", "symlink directory"} {
		t.Run(kind, func(t *testing.T) {
			path := layoutFor(t, src, root)
			algorithmDir := filepath.Join(path, "blobs", root.Digest.Algorithm().String())
			filename := filepath.Join(algorithmDir, root.Digest.Encoded())
			switch kind {
			case "directory leaf":
				if err := os.Remove(filename); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filename, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink directory":
				filename = algorithmDir
				fallthrough
			case "symlink leaf":
				target := filename + ".target"
				if err := os.Rename(filename, target); err != nil {
					t.Fatal(err)
				}
				symlinkLayoutFixture(t, filepath.Base(target), filename)
			}
			layout, _, err := readLayoutRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			defer layout.Close()
			_, fetchErr := content.FetchAll(t.Context(), layout.store, root)
			exists, existsErr := layout.store.Exists(t.Context(), root)
			if kind != "directory leaf" {
				if fetchErr != nil || !exists || existsErr != nil {
					t.Fatalf("contained blob link: fetch=%v exists=%v %v", fetchErr, exists, existsErr)
				}
				return
			}
			if !errors.Is(fetchErr, ErrIntegrity) || exists || !errors.Is(existsErr, ErrIntegrity) {
				t.Errorf("want integrity errors: fetch=%v exists=%v %v", fetchErr, exists, existsErr)
			}
		})
	}
}

func TestExternalLayoutCaptureMarkerPresence(t *testing.T) {
	src := memory.New()
	root := snapshotFor(t, src, []string{"example.test/bundle"}, []v1.Descriptor{object(t, src, "payload")}).Descriptor
	markerBytes := `{"schemaVersion":1,"bundle":"` + root.Digest.String() + `","sources":{}}`
	for _, kind := range []string{"absent", "regular", "contained link", "dangling link"} {
		t.Run(kind, func(t *testing.T) {
			path := layoutFor(t, src, root)
			marker := filepath.Join(path, captureFile)
			switch kind {
			case "regular":
				if err := os.WriteFile(marker, []byte(markerBytes), 0600); err != nil {
					t.Fatal(err)
				}
			case "contained link", "dangling link":
				target := captureFile + ".target"
				if kind == "contained link" {
					if err := os.WriteFile(filepath.Join(path, target), []byte(markerBytes), 0600); err != nil {
						t.Fatal(err)
					}
				}
				symlinkLayoutFixture(t, target, marker)
			}

			layout, _, err := readLayoutRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			defer layout.Close()
			raw, present, err := layout.captureMetadata()
			switch kind {
			case "absent":
				if raw != nil || present || err != nil {
					t.Fatalf("want absent marker: raw=%q present=%t err=%v", raw, present, err)
				}
			case "regular", "contained link":
				if string(raw) != markerBytes || !present || err != nil {
					t.Fatalf("want marker bytes: raw=%q present=%t err=%v", raw, present, err)
				}
			default:
				if raw != nil || present || !errors.Is(err, ErrIntegrity) || errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("want invalid, not absent marker: raw=%q present=%t err=%v", raw, present, err)
				}
				return
			}
			result, err := (&Service{}).InspectLayout(t.Context(), path)
			if err != nil || !content.Equal(result.Descriptor, root) || result.MetadataOnly != present {
				t.Fatalf("inspect marker %q: descriptor=%v metadataOnly=%t err=%v", kind, result.Descriptor, result.MetadataOnly, err)
			}
		})
	}
}

func TestExternalLayoutRejectsInvalidCaptureMarker(t *testing.T) {
	src := memory.New()
	root := snapshotFor(t, src, []string{"example.test/bundle"}, []v1.Descriptor{object(t, src, "payload")}).Descriptor
	path := layoutFor(t, src, root)
	if err := os.Mkdir(filepath.Join(path, captureFile), 0700); err != nil {
		t.Fatal(err)
	}

	svc := &Service{}
	ctx := t.Context()
	_, inspectErr := svc.InspectLayout(ctx, path)
	_, saveErr := svc.SaveTo(ctx, path, io.Discard)
	_, planErr := svc.PlanPush(ctx, path, "example.test/release:stable", PushOptions{})
	_, diffErr := svc.DiffLayouts(ctx, path, path)
	_, exportErr := svc.ExportDeliveryLayout(ctx, path)
	_, updateErr := svc.UpdateDeliveryLayouts(ctx, path, path, deliveryfile.File{})
	for operation, err := range map[string]error{
		"inspect": inspectErr, "save": saveErr, "plan": planErr,
		"diff": diffErr, "export": exportErr, "update": updateErr,
	} {
		if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), captureFile) {
			t.Errorf("%s: want integrity error for %s, got %v", operation, captureFile, err)
		}
	}
}
