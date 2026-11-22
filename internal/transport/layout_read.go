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
	_ = l.files.root.Close()
}

// layoutFiles implements only Open so ORAS Fetch and fs.Stat (used by
// Exists) both go through the same descriptor-validated regular-file policy.
type layoutFiles struct {
	root *os.Root
}

func (f layoutFiles) Open(name string) (fs.File, error) {
	file, err := f.open(name)
	if err != nil {
		return nil, err
	}
	if _, err := statRegularLayoutFile(name, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (f layoutFiles) open(name string) (*os.File, error) {
	pathErr := func(err error) error { return &fs.PathError{Op: "open", Path: name, Err: err} }
	if name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, "\\:\x00") {
		return nil, pathErr(fs.ErrInvalid)
	}
	file, err := f.openLayoutFile(name)
	if err != nil {
		return nil, pathErr(err)
	}
	return file, nil
}

func statRegularLayoutFile(name string, file *os.File) (fs.FileInfo, error) {
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%w: not a regular file", ErrIntegrity)
	}
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return info, nil
}

func (l *openedLayout) readBoundedRegularFile(name string, maxBytes int64) ([]byte, error) {
	file, err := l.files.open(name)
	if err != nil {
		return nil, err
	}
	return readBoundedLayoutFile(name, file, maxBytes)
}

// readBoundedLayoutFile takes ownership of file and limits actual bytes read
// in case a regular file grows after the size check.
func readBoundedLayoutFile(name string, file *os.File, maxBytes int64) ([]byte, error) {
	defer func() { _ = file.Close() }()
	info, err := statRegularLayoutFile(name, file)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, maxBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
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
	files, err := openLayoutFiles(path)
	if err != nil {
		return nil, v1.Descriptor{}, err
	}
	layout := &openedLayout{files: files}
	root, err := layout.descriptor()
	if err != nil {
		layout.Close()
		return nil, v1.Descriptor{}, err
	}
	layout.store = oci.NewStorageFromFS(layout.files)
	return layout, root, nil
}
