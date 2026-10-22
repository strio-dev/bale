package transport

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"oras.land/oras-go/v2/content/oci"
)

// ArchiveResult identifies the unchanged bundle carried by an OCI layout tar.
type ArchiveResult struct {
	Descriptor  v1.Descriptor `json:"descriptor"`
	LayoutPath  string        `json:"layoutPath,omitempty"`
	ArchivePath string        `json:"archivePath,omitempty"`
}

// Save writes the required bundle closure as a deterministic, uncompressed OCI
// layout tar. Unrelated files in the input layout are never archived.
func (s *Service) Save(ctx context.Context, layoutPath, archivePath string) (result ArchiveResult, retErr error) {
	defer func() { retErr = operationError("save", layoutPath, result.Descriptor, nil, retErr) }()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := checkArchiveDestination(archivePath); err != nil {
		return result, err
	}
	store, snapshot, err := openLayout(ctx, layoutPath)
	if err != nil {
		return result, err
	}
	result.Descriptor = snapshot.Descriptor
	parent := filepath.Dir(filepath.Clean(archivePath))
	if err := os.MkdirAll(parent, 0755); err != nil {
		return result, err
	}
	stage, err := os.MkdirTemp(parent, ".bale-save-")
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(stage)) }()
	normalized := filepath.Join(stage, "layout")
	if err := writeLayout(ctx, store, snapshot.Descriptor, normalized, &Stats{}); err != nil {
		return result, err
	}
	fp, err := os.OpenFile(filepath.Join(stage, "archive.tar"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return result, err
	}
	if err = writeLayoutTar(ctx, fp, normalized); err == nil {
		err = fp.Sync()
	}
	err = errors.Join(err, fp.Close())
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := os.Link(filepath.Join(stage, "archive.tar"), archivePath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return result, fmt.Errorf("%w: archive path already exists: %s", ErrConflict, archivePath)
		}
		return result, err
	}
	result.ArchivePath = archivePath
	result.LayoutPath = layoutPath
	return result, nil
}

// Load validates a portable OCI tar before publishing a complete local layout.
// The archive is staged privately: ORAS may reopen its path while walking the
// graph, and must not observe changes made to the caller's archive meanwhile.
func (s *Service) Load(ctx context.Context, archivePath, layoutPath string) (result ArchiveResult, retErr error) {
	defer func() { retErr = operationError("load", archivePath, result.Descriptor, nil, retErr) }()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := checkLayoutDestination(layoutPath); err != nil {
		return result, err
	}
	stage, err := os.MkdirTemp("", "bale-load-")
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(stage)) }()
	staged := filepath.Join(stage, "archive.tar")
	if err := stageArchive(ctx, archivePath, staged); err != nil {
		return result, err
	}
	root, err := scanLayoutTar(ctx, staged)
	if err != nil {
		return result, err
	}
	result.Descriptor = root
	store, err := oci.NewStorageFromTar(staged)
	if err != nil {
		return result, err
	}
	if _, err := bundle.LoadSnapshotGraph(ctx, store, root); err != nil {
		return result, err
	}
	if err := writeLayout(ctx, store, root, layoutPath, &Stats{}); err != nil {
		return result, err
	}
	result.LayoutPath = layoutPath
	result.ArchivePath = archivePath
	return result, nil
}

func checkArchiveDestination(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: archive path is required", ErrInvalid)
	}
	if _, err := os.Lstat(name); err == nil {
		return fmt.Errorf("%w: archive path already exists: %s", ErrConflict, name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func stageArchive(ctx context.Context, source, target string) (retErr error) {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: archive must be a regular file", ErrInvalid)
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, in.Close()) }()
	info, err = in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: archive must be a regular file", ErrInvalid)
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, out.Close()) }()
	_, err = io.Copy(out, &archiveContextReader{ctx: ctx, r: in})
	return err
}

func writeLayoutTar(ctx context.Context, out io.Writer, layout string) (retErr error) {
	var names []string
	err := filepath.WalkDir(layout, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%w: nonregular layout member", ErrInvalid)
		}
		rel, err := filepath.Rel(layout, name)
		if err != nil {
			return err
		}
		names = append(names, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(names)
	tw := tar.NewWriter(out)
	defer func() { retErr = errors.Join(retErr, tw.Close()) }()
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		fp, err := os.Open(filepath.Join(layout, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		info, err := fp.Stat()
		if err == nil {
			err = tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: info.Size(), Typeflag: tar.TypeReg, Format: tar.FormatPAX})
		}
		if err == nil {
			_, err = io.Copy(tw, &archiveContextReader{ctx: ctx, r: fp})
		}
		err = errors.Join(err, fp.Close())
		if err != nil {
			return err
		}
	}
	return nil
}

type archiveContextReader struct {
	ctx context.Context
	r   io.Reader
	n   int64
}

func (r *archiveContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

// ORAS tar storage indexes entries without rejecting duplicate names or cleaned
// traversal paths. Validate those boundaries and all stored blob bytes first.
func scanLayoutTar(ctx context.Context, name string) (root v1.Descriptor, retErr error) {
	fp, err := os.Open(name)
	if err != nil {
		return root, err
	}
	defer func() { retErr = errors.Join(retErr, fp.Close()) }()
	cr := &archiveContextReader{ctx: ctx, r: fp}
	tr := tar.NewReader(cr)
	seen := map[string]bool{}
	var layoutRaw, indexRaw []byte
	var end int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			if cr.n < end+1024 {
				return root, fmt.Errorf("%w: missing tar end markers", ErrIntegrity)
			}
			// A tar may have additional zero record padding, but no concatenated
			// archive or hidden trailing content.
			buf := make([]byte, 32*1024)
			for {
				n, e := cr.Read(buf)
				if !allZero(buf[:n]) {
					return root, fmt.Errorf("%w: nonzero tar trailer", ErrIntegrity)
				}
				if e != nil {
					if !errors.Is(e, io.EOF) {
						return root, e
					}
					break
				}
			}
			break
		}
		if err != nil {
			return root, fmt.Errorf("%w: %w", ErrIntegrity, err)
		}
		member := h.Name
		if strings.Contains(member, "\\") || path.IsAbs(member) {
			return root, fmt.Errorf("%w: unsafe tar member %q", ErrInvalid, h.Name)
		}
		// Standard filesystem tar tools commonly prefix every path with ./.
		// Only that harmless prefix is normalized; interior traversal and
		// noncanonical path components remain invalid.
		for strings.HasPrefix(member, "./") {
			member = strings.TrimPrefix(member, "./")
		}
		member = strings.TrimSuffix(member, "/")
		if member == "" && h.Typeflag == tar.TypeDir {
			member = "."
		}
		if path.Clean(member) != member || seen[member] {
			return root, fmt.Errorf("%w: unsafe or duplicate tar member %q", ErrInvalid, h.Name)
		}
		seen[member] = true
		if h.Typeflag == tar.TypeDir {
			// ORAS layouts also contain an empty ingest directory. No ingest
			// files are accepted or copied into the published layout.
			clean := strings.TrimSuffix(member, "/")
			if h.Size != 0 || (clean != "." && clean != "blobs" && clean != "ingest" && !validArchiveAlgorithmDirectory(clean)) {
				return root, fmt.Errorf("%w: unexpected tar directory %q", ErrInvalid, member)
			}
			end = cr.n
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return root, fmt.Errorf("%w: unsupported tar entry %q", ErrInvalid, member)
		}
		switch member {
		case "oci-layout", "index.json":
			const maxArchiveMetadata = 8 << 20
			if h.Size > maxArchiveMetadata {
				return root, fmt.Errorf("%w: OCI layout metadata exceeds 8 MiB", ErrInvalid)
			}
			raw, err := io.ReadAll(tr)
			if err != nil {
				return root, fmt.Errorf("%w: %w", ErrIntegrity, err)
			}
			if member == "oci-layout" {
				layoutRaw = raw
			} else {
				indexRaw = raw
			}
		default:
			parts := strings.Split(member, "/")
			if len(parts) != 3 || parts[0] != "blobs" {
				return root, fmt.Errorf("%w: unexpected OCI tar member %q", ErrInvalid, member)
			}
			dgst, err := digest.Parse(parts[1] + ":" + parts[2])
			if err != nil || !dgst.Algorithm().Available() {
				return root, fmt.Errorf("%w: invalid blob path %q", ErrInvalid, member)
			}
			verifier := dgst.Verifier()
			n, err := io.CopyN(verifier, tr, h.Size)
			if err != nil {
				return root, fmt.Errorf("%w: %w", ErrIntegrity, err)
			}
			if n != h.Size || !verifier.Verified() {
				return root, fmt.Errorf("%w: corrupt blob %s", ErrIntegrity, dgst)
			}
		}
		end = cr.n + (512-h.Size%512)%512
	}
	var layout v1.ImageLayout
	if err := json.Unmarshal(layoutRaw, &layout); err != nil || layout.Version != v1.ImageLayoutVersion {
		return root, fmt.Errorf("%w: unsupported OCI layout", ErrInvalid)
	}
	var index v1.Index
	if err := json.Unmarshal(indexRaw, &index); err != nil || index.SchemaVersion != 2 || len(index.Manifests) != 1 || (index.MediaType != "" && index.MediaType != v1.MediaTypeImageIndex) || index.Subject != nil || index.ArtifactType != "" {
		return root, fmt.Errorf("%w: archive requires one OCI bundle root", ErrInvalid)
	}
	return index.Manifests[0], nil
}

func validArchiveAlgorithmDirectory(name string) bool {
	parts := strings.Split(name, "/")
	return len(parts) == 2 && parts[0] == "blobs" && digest.Algorithm(parts[1]).Available()
}

func allZero(raw []byte) bool {
	for _, b := range raw {
		if b != 0 {
			return false
		}
	}
	return true
}
