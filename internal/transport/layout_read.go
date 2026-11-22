package transport

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
)

const maxLayoutHeaderBytes = 8 << 20

// openedLayout owns the directory opened for one input. The ORAS store
// borrows it and must not outlive it.
type openedLayout struct {
	files layoutFiles
	store content.ReadOnlyStorage
}

func (l *openedLayout) Close() { _ = l.files.directory.Close() }

// layoutFiles implements only Open so ORAS Fetch and fs.Stat (used by
// Exists) both go through the same descriptor-validated regular-file policy.
type layoutFiles struct{ directory *os.File }

func (f layoutFiles) Open(name string) (fs.File, error) {
	file, _, err := f.openRegular(name)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (f layoutFiles) openRegular(name string) (*os.File, fs.FileInfo, error) {
	pathErr := func(err error) error { return &fs.PathError{Op: "open", Path: name, Err: err} }
	if name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, "\\:\x00") {
		return nil, nil, pathErr(fs.ErrInvalid)
	}
	parent, remaining := f.directory, name
	for {
		part, rest, more := strings.Cut(remaining, "/")
		file, err := openLayoutMemberAt(parent, part, more)
		if parent != f.directory {
			_ = parent.Close()
		}
		if err != nil {
			return nil, nil, pathErr(err)
		}
		if !more {
			info, err := file.Stat()
			if err == nil && !info.Mode().IsRegular() {
				err = fmt.Errorf("%w: not a regular file", ErrIntegrity)
			}
			if err != nil {
				_ = file.Close()
				return nil, nil, pathErr(err)
			}
			return file, info, nil
		}
		parent, remaining = file, rest
	}
}

// readBoundedRegularFile also limits actual bytes read in case a regular file
// grows after the size check.
func (l *openedLayout) readBoundedRegularFile(name string, maxBytes int64) ([]byte, error) {
	fp, info, err := l.files.openRegular(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fp.Close() }()
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, maxBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(fp, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, maxBytes)
	}
	return raw, nil
}

func (l *openedLayout) descriptor() (v1.Descriptor, error) {
	raw, err := l.readBoundedRegularFile("oci-layout", maxLayoutHeaderBytes)
	if err != nil {
		return v1.Descriptor{}, err
	}
	if err := decodeLayoutMarker(raw); err != nil {
		return v1.Descriptor{}, err
	}
	raw, err = l.readBoundedRegularFile("index.json", maxLayoutHeaderBytes)
	if err != nil {
		return v1.Descriptor{}, err
	}
	return decodeLayoutIndex(raw)
}

type managedLayoutCASKey string

type managedLayoutCAS struct{ store *oci.Storage }

// WithManagedLayoutCAS binds a cooperating store session's CAS to its exact
// private view path. The view's headers are still read through the directory
// handle; blob reads use the already-open CAS instead of traversing its link.
// The store session must outlive use of the returned context.
func WithManagedLayoutCAS(ctx context.Context, viewPath string, cas *oci.Storage) context.Context {
	return context.WithValue(ctx, managedLayoutCASKey(viewPath), managedLayoutCAS{store: cas})
}

// A successful caller owns the returned layout. On failure no handle escapes.
func readLayoutRoot(ctx context.Context, path string) (*openedLayout, v1.Descriptor, error) {
	dir, err := openLayoutDirectory(path)
	if err != nil {
		return nil, v1.Descriptor{}, err
	}
	layout := &openedLayout{files: layoutFiles{dir}}
	root, err := layout.descriptor()
	if err != nil {
		layout.Close()
		return nil, v1.Descriptor{}, err
	}
	if cas, ok := ctx.Value(managedLayoutCASKey(path)).(managedLayoutCAS); ok && cas.store != nil {
		layout.store = cas.store
	} else {
		layout.store = oci.NewStorageFromFS(layout.files)
	}
	return layout, root, nil
}
