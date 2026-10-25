// Package localstore owns Bale's shared OCI content and local root references.
package localstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/jsonutil"
	"github.com/strio-dev/bale/internal/transport"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

const maxMetadataBytes = 64 << 20
const maxCaptureMetadataBytes = 16 << 20
const captureName = "bale-capture.json"

type Store struct {
	path    string
	storage *oci.Storage
}
type Session struct{ store *Store }
type Entry struct {
	Descriptor   v1.Descriptor `json:"descriptor"`
	Names        []string      `json:"names"`
	MetadataOnly bool          `json:"metadataOnly"`
}
type Record struct {
	Entry
	Capture *transport.CaptureState
}
type index struct {
	roots map[digest.Digest]v1.Descriptor
	names map[string]digest.Digest
}
type PruneResult struct {
	Blobs    int   `json:"blobs"`
	Bytes    int64 `json:"bytes"`
	Captures int   `json:"captures"`
}

func DefaultPath() (string, error) {
	if path := os.Getenv("XDG_DATA_HOME"); path != "" {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("XDG_DATA_HOME must be an absolute path")
		}
		return filepath.Join(path, "bale"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "bale"), nil
}

func Open(path string) (*Store, error) { return OpenContext(context.Background(), path) }

func OpenContext(ctx context.Context, path string) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var err error
	if path == "" {
		path, err = DefaultPath()
		if err != nil {
			return nil, err
		}
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	storage, err := oci.NewStorage(path)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, storage: storage}
	if err := s.initialize(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// initialize publishes an empty index only for a genuinely new store. A missing
// index in an existing store is data loss, never permission to sweep all blobs.
func (s *Store) initialize(ctx context.Context) (retErr error) {
	activity, err := lock(ctx, filepath.Join(s.path, ".activity.lock"), false)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, activity.Close()) }()
	metadata, err := lock(ctx, filepath.Join(s.path, ".metadata.lock"), true)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, metadata.Close()) }()
	if _, err := os.Lstat(filepath.Join(s.path, "index.json")); err == nil {
		_, err = (&Session{store: s}).readIndex()
		return err
	} else if !os.IsNotExist(err) {
		return err
	}
	entries, err := os.ReadDir(s.path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != ".activity.lock" && entry.Name() != ".metadata.lock" {
			return fmt.Errorf("managed store index is missing from nonempty store %s", s.path)
		}
	}
	if err := atomicWrite(filepath.Join(s.path, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return err
	}
	return (&Session{store: s}).writeIndex(ctx, &index{roots: map[digest.Digest]v1.Descriptor{}, names: map[string]digest.Digest{}})
}
func (s *Store) Path() string            { return s.path }
func (s *Session) Storage() *oci.Storage { return s.store.storage }

// With prevents garbage collection while the callback uses any store content.
func (s *Store) With(ctx context.Context, fn func(*Session) error) (retErr error) {
	fp, err := lock(ctx, filepath.Join(s.path, ".activity.lock"), false)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, fp.Close()) }()
	return fn(&Session{store: s})
}

func (s *Session) metadata(ctx context.Context, fn func(*index) error) (retErr error) {
	fp, err := lock(ctx, filepath.Join(s.store.path, ".metadata.lock"), true)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, fp.Close()) }()
	current, err := s.readIndex()
	if err != nil {
		return err
	}
	return fn(current)
}

func readFile(path string) ([]byte, error) {
	entry, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !entry.Mode().IsRegular() || entry.Size() > maxMetadataBytes {
		return nil, fmt.Errorf("invalid store metadata file %s", path)
	}
	fp, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fp.Close() }()
	info, err := fp.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxMetadataBytes {
		return nil, fmt.Errorf("invalid store metadata file %s", path)
	}
	raw, err := io.ReadAll(io.LimitReader(fp, maxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxMetadataBytes {
		return nil, fmt.Errorf("store metadata exceeds limit")
	}
	return raw, nil
}
func (s *Session) readIndex() (*index, error) {
	root := s.store.path
	layoutPath := filepath.Join(root, "oci-layout")
	raw, err := os.ReadFile(layoutPath)
	if err != nil {
		return nil, err
	}
	var layout v1.ImageLayout
	if err := json.Unmarshal(raw, &layout); err != nil || layout.Version != v1.ImageLayoutVersion {
		return nil, fmt.Errorf("invalid managed OCI layout")
	}
	result := &index{roots: map[digest.Digest]v1.Descriptor{}, names: map[string]digest.Digest{}}
	raw, err = readFile(filepath.Join(root, "index.json"))
	if err != nil {
		return nil, err
	}
	if err = jsonutil.Validate(raw, true); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var value v1.Index
	if err = dec.Decode(&value); err != nil {
		return nil, err
	}
	if value.SchemaVersion != 2 || value.MediaType != v1.MediaTypeImageIndex {
		return nil, fmt.Errorf("invalid managed OCI index")
	}
	for _, d := range value.Manifests {
		if d.Digest.Validate() != nil || !d.Digest.Algorithm().Available() || d.Size < 0 || d.MediaType != v1.MediaTypeImageIndex {
			return nil, fmt.Errorf("invalid retained bundle descriptor")
		}
		if old, ok := result.roots[d.Digest]; ok && (old.Size != d.Size || old.MediaType != d.MediaType) {
			return nil, fmt.Errorf("conflicting retained root %s", d.Digest)
		}
		name := d.Annotations[v1.AnnotationRefName]
		delete(d.Annotations, v1.AnnotationRefName)
		if len(d.Annotations) == 0 {
			d.Annotations = nil
		}
		result.roots[d.Digest] = d
		if name != "" {
			canonical, err := NormalizeTag(name)
			if err != nil || canonical != name {
				return nil, fmt.Errorf("invalid stored name %q", name)
			}
			if _, exists := result.names[name]; exists {
				return nil, fmt.Errorf("duplicate stored name %q", name)
			}
			result.names[name] = d.Digest
		}
	}
	return result, nil
}
func (s *Session) writeIndex(ctx context.Context, current *index) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	value := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{}}
	roots := make([]string, 0, len(current.roots))
	for d := range current.roots {
		roots = append(roots, d.String())
	}
	sort.Strings(roots)
	for _, d := range roots {
		value.Manifests = append(value.Manifests, current.roots[digest.Digest(d)])
	}
	names := make([]string, 0, len(current.names))
	for name := range current.names {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		d := current.roots[current.names[name]]
		d.Annotations = cloneAnnotations(d.Annotations)
		d.Annotations[v1.AnnotationRefName] = name
		value.Manifests = append(value.Manifests, d)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > maxMetadataBytes {
		return fmt.Errorf("managed OCI index exceeds limit")
	}
	return atomicWrite(filepath.Join(s.store.path, "index.json"), raw)
}
func cloneAnnotations(input map[string]string) map[string]string {
	output := map[string]string{}
	for k, v := range input {
		output[k] = v
	}
	return output
}
func atomicWrite(path string, raw []byte) (retErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	fp, err := os.CreateTemp(filepath.Dir(path), ".bale-write-")
	if err != nil {
		return err
	}
	name := fp.Name()
	defer func() {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			retErr = errors.Join(retErr, err)
		}
	}()
	if _, err = fp.Write(raw); err == nil {
		err = fp.Sync()
	}
	err = errors.Join(err, fp.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
func capturePath(root string, d digest.Digest) string {
	return filepath.Join(root, "captures", d.Algorithm().String(), d.Encoded()+".json")
}
func (s *Session) state(d digest.Digest) (*transport.CaptureState, error) {
	raw, err := readFile(capturePath(s.store.path, d))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > maxCaptureMetadataBytes {
		return nil, fmt.Errorf("capture metadata exceeds limit")
	}
	if err = jsonutil.Validate(raw, true); err != nil {
		return nil, err
	}
	var state transport.CaptureState
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&state); err != nil {
		return nil, err
	}
	return &state, nil
}
func (s *Session) record(ctx context.Context, current *index, d digest.Digest) (Record, error) {
	root, ok := current.roots[d]
	if !ok {
		return Record{}, &transport.Error{Op: "resolve local bundle", Kind: transport.ErrNotFound, Cause: fmt.Errorf("%w: %s", errdef.ErrNotFound, d)}
	}
	snapshot, inventory, err := bundle.InspectSnapshotInventory(ctx, s.Storage(), root)
	if err != nil {
		return Record{}, err
	}
	state, err := s.state(d)
	if err != nil {
		return Record{}, err
	}
	if state != nil {
		if err := transport.ValidateCaptureState(ctx, s.Storage(), snapshot, *state); err != nil {
			return Record{}, err
		}
	}
	missing := false
	for _, descriptor := range inventory {
		if descriptor.Data != nil {
			continue
		}
		exists, err := s.Storage().Exists(ctx, descriptor)
		if err != nil {
			return Record{}, err
		}
		missing = missing || !exists
	}
	if missing && state == nil {
		return Record{}, fmt.Errorf("retained complete bundle %s has missing content", d)
	}
	names := []string{}
	for name, id := range current.names {
		if id == d {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return Record{Entry: Entry{Descriptor: root, Names: names, MetadataOnly: missing}, Capture: state}, nil
}
func resolve(current *index, value string) (id digest.Digest, name string, err error) {
	// Exact local names take precedence over bare IDs and digest-looking tags.
	if normalized, normalizeErr := NormalizeTag(value); normalizeErr == nil {
		if target, exists := current.names[normalized]; exists {
			id, name = target, normalized
		}
	}
	if id == "" {
		id, name, err = selector(value)
		if err != nil {
			return "", "", err
		}
		if id == "" {
			prefix, prefixErr := shortID(value)
			if prefixErr != nil {
				return "", "", prefixErr
			}
			if prefix != "" {
				name = ""
				for candidate := range current.roots {
					if strings.HasPrefix(candidate.String(), prefix) {
						if id != "" {
							return "", "", fmt.Errorf("ambiguous bundle ID %q; use a longer digest", value)
						}
						id = candidate
					}
				}
			}
		}
	}
	if _, ok := current.roots[id]; !ok {
		return "", "", &transport.Error{Op: "resolve local bundle", Kind: transport.ErrNotFound, Cause: fmt.Errorf("%w: %q", errdef.ErrNotFound, value)}
	}
	return id, name, nil
}
func (s *Session) Resolve(ctx context.Context, value string) (result Record, retErr error) {
	retErr = s.metadata(ctx, func(current *index) error {
		id, _, err := resolve(current, value)
		if err != nil {
			return err
		}
		result, err = s.record(ctx, current, id)
		return err
	})
	return
}
func (s *Session) List(ctx context.Context) (result []Entry, retErr error) {
	result = []Entry{}
	retErr = s.metadata(ctx, func(current *index) error {
		ids := make([]string, 0, len(current.roots))
		for id := range current.roots {
			ids = append(ids, id.String())
		}
		sort.Strings(ids)
		for _, id := range ids {
			r, err := s.record(ctx, current, digest.Digest(id))
			if err != nil {
				return err
			}
			result = append(result, r.Entry)
		}
		return nil
	})
	return
}
func (s *Session) Publish(ctx context.Context, root v1.Descriptor, state *transport.CaptureState, tags []string) error {
	names := make([]string, len(tags))
	for i, name := range tags {
		var err error
		names[i], err = NormalizeTag(name)
		if err != nil {
			return err
		}
	}
	snapshot, _, err := bundle.InspectSnapshotInventory(ctx, s.Storage(), root)
	if err != nil {
		return err
	}
	if state != nil {
		if err := transport.ValidateCaptureState(ctx, s.Storage(), snapshot, *state); err != nil {
			return err
		}
	} else {
		if _, err := bundle.LoadSnapshotGraph(ctx, s.Storage(), root); err != nil {
			return err
		}
	}
	return s.metadata(ctx, func(current *index) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if state != nil {
			old, err := s.state(root.Digest)
			if err != nil {
				return err
			}
			merged := *state
			merged.Sources = map[digest.Digest]string{}
			for d, repo := range state.Sources {
				merged.Sources[d] = repo
			}
			if old != nil {
				for d, repo := range old.Sources {
					merged.Sources[d] = repo
				}
			}
			if err := transport.ValidateCaptureState(ctx, s.Storage(), snapshot, merged); err != nil {
				return err
			}
			raw, err := json.Marshal(merged)
			if err != nil {
				return err
			}
			if len(raw) > maxCaptureMetadataBytes {
				return fmt.Errorf("capture metadata exceeds limit")
			}
			if err := atomicWrite(capturePath(s.store.path, root.Digest), raw); err != nil {
				return err
			}
		}
		root.Annotations = cloneAnnotations(root.Annotations)
		delete(root.Annotations, v1.AnnotationRefName)
		if len(root.Annotations) == 0 {
			root.Annotations = nil
		}
		current.roots[root.Digest] = root
		for _, name := range names {
			current.names[name] = root.Digest
		}
		return s.writeIndex(ctx, current)
	})
}
func (s *Session) Tag(ctx context.Context, reference string, names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("at least one local name is required")
	}
	normalized := make([]string, len(names))
	for i, name := range names {
		var err error
		normalized[i], err = NormalizeTag(name)
		if err != nil {
			return err
		}
	}
	return s.metadata(ctx, func(current *index) error {
		id, _, err := resolve(current, reference)
		if err != nil {
			return err
		}
		for _, name := range normalized {
			current.names[name] = id
		}
		return s.writeIndex(ctx, current)
	})
}
func (s *Session) Remove(ctx context.Context, references []string) error {
	if len(references) == 0 {
		return fmt.Errorf("at least one local reference is required")
	}
	return s.metadata(ctx, func(current *index) error {
		for _, reference := range references {
			id, name, err := resolve(current, reference)
			if err != nil {
				return err
			}
			if name == "" {
				delete(current.roots, id)
				for alias, target := range current.names {
					if target == id {
						delete(current.names, alias)
					}
				}
				continue
			}
			delete(current.names, name)
			named := false
			for _, target := range current.names {
				named = named || target == id
			}
			if !named {
				delete(current.roots, id)
			}
		}
		return s.writeIndex(ctx, current)
	})
}

// View exposes only disposable index/locator metadata, sharing the global CAS.
func (s *Session) View(ctx context.Context, record Record, fn func(string) error) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	parent := filepath.Join(s.store.path, "views")
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	path, err := os.MkdirTemp(parent, "read-")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(path)) }()
	raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{record.Descriptor}})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(path, "index.json"), raw, 0600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(path, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0600); err != nil {
		return err
	}
	if err := os.Symlink(filepath.Join(s.store.path, "blobs"), filepath.Join(path, "blobs")); err != nil {
		return err
	}
	if record.MetadataOnly {
		raw, err := json.Marshal(record.Capture)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(path, captureName), raw, 0600); err != nil {
			return err
		}
	}
	return fn(path)
}
