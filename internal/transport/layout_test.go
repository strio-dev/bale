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
	"oras.land/oras-go/v2/content/oci"
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
					if err := os.Symlink(filepath.Base(target), filename); err != nil {
						t.Fatal(err)
					}
				}

				_, saveErr := svc.SaveTo(t.Context(), layout, io.Discard)
				_, inspectErr := svc.InspectLayout(t.Context(), layout)
				checkStandaloneHeaderError(t, header, saveErr, inspectErr)
			})
		}
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
	layout, root, err := readLayoutRoot(t.Context(), path)
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
		layout, got, err := readLayoutRoot(t.Context(), path)
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
				if err := os.Symlink(filepath.Base(target), filename); err != nil {
					if errors.Is(err, fs.ErrPermission) {
						t.Skipf("creating symlinks is not permitted: %v", err)
					}
					t.Fatal(err)
				}
			}
			store, _, err := openLayoutTestRoot(t, path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := content.FetchAll(t.Context(), store, root); !errors.Is(err, ErrIntegrity) {
				t.Errorf("Fetch: want integrity error, got %v", err)
			}
			if exists, err := store.Exists(t.Context(), root); exists || !errors.Is(err, ErrIntegrity) {
				t.Errorf("Exists: want integrity error, got exists=%v err=%v", exists, err)
			}
		})
	}
}

func TestManagedLayoutCASIsBoundToItsView(t *testing.T) {
	src := memory.New()
	root := snapshotFor(t, src, []string{"example.test/bundle"}, []v1.Descriptor{object(t, src, "payload")}).Descriptor
	view, other := layoutFor(t, src, root), layoutFor(t, src, root)
	cas, err := oci.NewStorage(view)
	if err != nil {
		t.Fatal(err)
	}

	ctx := WithManagedLayoutCAS(t.Context(), view, cas)
	for _, tc := range []struct {
		ctx     context.Context
		path    string
		managed bool
	}{
		{ctx, view, true},
		{ctx, other, false},
		{t.Context(), view, false},
		{context.WithValue(t.Context(), managedLayoutCASKey(view), cas), view, false},
	} {
		layout, _, err := readLayoutRoot(tc.ctx, tc.path)
		if err != nil {
			t.Fatal(err)
		}
		layout.Close()
		if (layout.store == cas) != tc.managed {
			t.Errorf("CAS reuse for %s: want managed=%v", tc.path, tc.managed)
		}
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
