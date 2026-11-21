package transport

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
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
