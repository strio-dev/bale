package transport

import (
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

func (l *openedLayout) Close() {
	if l.files.memberRoot != nil {
		_ = l.files.memberRoot.Close()
	}
	_ = l.files.directory.Close()
}

// layoutFiles implements only Open so ORAS Fetch and fs.Stat (used by
// Exists) both go through the same descriptor-validated regular-file policy.
type layoutFiles struct {
	directory  *os.File
	memberRoot *layoutMemberRoot
}

type layoutMemberRoot struct {
	root  *os.Root
	flags int
}

func (r *layoutMemberRoot) Close() error { return r.root.Close() }

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
	var file *os.File
	var err error
	// Only the OCI headers and blobs may use rooted relative links. The
	// optional capture marker uses no-follow so an invalid link is not missing.
	if f.memberRoot != nil && (name == "oci-layout" || name == "index.json" || strings.HasPrefix(name, "blobs/")) {
		file, err = f.memberRoot.root.OpenFile(name, f.memberRoot.flags, 0)
	} else {
		file, err = f.openNoFollow(name)
	}
	if err != nil {
		return nil, nil, pathErr(err)
	}
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

// openNoFollow performs strict single-component lookup for capture metadata
// and for platforms without rooted link handling.
func (f layoutFiles) openNoFollow(name string) (*os.File, error) {
	parent, remaining := f.directory, name
	for {
		part, rest, more := strings.Cut(remaining, "/")
		file, err := openLayoutMemberAt(parent, part, more)
		if parent != f.directory {
			_ = parent.Close()
		}
		if err != nil || !more {
			return file, err
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

// A successful caller owns the returned layout. On failure no handle escapes.
func readLayoutRoot(path string) (*openedLayout, v1.Descriptor, error) {
	dir, err := openLayoutDirectory(path)
	if err != nil {
		return nil, v1.Descriptor{}, err
	}
	layout := &openedLayout{files: layoutFiles{directory: dir}}
	layout.files.memberRoot, err = openLayoutMemberRoot(path, dir)
	if err != nil {
		layout.Close()
		return nil, v1.Descriptor{}, err
	}
	root, err := layout.descriptor()
	if err != nil {
		layout.Close()
		return nil, v1.Descriptor{}, err
	}
	layout.store = oci.NewStorageFromFS(layout.files)
	return layout, root, nil
}
