package pluginoci

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/strio-dev/bale/internal/yamlutil"
	"go.yaml.in/yaml/v3"
)

const maxArchiveEntries = 4096

type packageMetadata struct{ name, version string }

func (m packageMetadata) title() string { return m.name + "-" + m.version + ".tgz" }

// readArchive only writes the verified module candidate to output. All paths,
// metadata, ancillary files, trailers and resource limits must validate before
// callers can use that candidate. Ancillary files remain in the original blob.
func readArchive(ctx context.Context, source io.Reader, output io.Writer) (packageMetadata, error) {
	compressedLimit := &io.LimitedReader{R: contextReader{ctx, source}, N: MaxArchiveBytes + 1}
	compressed := bufio.NewReader(compressedLimit)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return packageMetadata{}, err
	}
	defer func() { _ = gz.Close() }()
	gz.Multistream(false)
	expandedLimit := &io.LimitedReader{R: gz, N: MaxArchiveBytes + 1}
	counter := &archiveCounter{reader: expandedLimit}
	tr := tar.NewReader(counter)
	seen := map[string]bool{}
	var root string
	var metadataRaw []byte
	var moduleFound bool
	var end int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			if counter.n < end+1024 {
				return packageMetadata{}, errors.New("plugin archive lacks complete tar end markers")
			}
			break
		}
		if err != nil {
			return packageMetadata{}, err
		}
		if len(seen) >= maxArchiveEntries {
			return packageMetadata{}, errors.New("plugin archive exceeds 4096 entries")
		}
		member := h.Name
		if strings.Contains(member, "\\") || path.IsAbs(member) || len(member) > 4096 {
			return packageMetadata{}, fmt.Errorf("unsafe plugin archive path %q", member)
		}
		for strings.HasPrefix(member, "./") {
			member = strings.TrimPrefix(member, "./")
		}
		if h.Typeflag == tar.TypeDir {
			member = strings.TrimSuffix(member, "/")
		}
		if member == "" || member == "." || path.Clean(member) != member || strings.HasPrefix(member, "../") || seen[member] {
			return packageMetadata{}, fmt.Errorf("unsafe or duplicate plugin archive path %q", h.Name)
		}
		seen[member] = true
		parts := strings.Split(member, "/")
		if root == "" {
			root = parts[0]
		}
		if parts[0] != root || (len(parts) == 1 && h.Typeflag != tar.TypeDir) {
			return packageMetadata{}, errors.New("plugin archive must contain one package directory")
		}
		if h.Mode&0o6000 != 0 {
			return packageMetadata{}, errors.New("plugin archive contains setuid/setgid permissions")
		}
		if h.Typeflag == tar.TypeDir {
			if h.Size != 0 {
				return packageMetadata{}, errors.New("plugin archive directory has content")
			}
			end = counter.n
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return packageMetadata{}, errors.New("plugin archive contains a link or special file")
		}
		if h.Size < 0 || h.Size > MaxArchiveBytes-counter.n {
			return packageMetadata{}, errors.New("plugin archive exceeds 256 MiB expanded")
		}
		switch member {
		case root + "/plugin.yaml":
			if h.Size > MaxMetadataBytes {
				return packageMetadata{}, errors.New("plugin.yaml exceeds 4 MiB")
			}
			metadataRaw, err = io.ReadAll(tr)
		case root + "/plugin.wasm":
			if h.Size < int64(len(coreHeader)) || h.Size > MaxModuleBytes {
				return packageMetadata{}, errors.New("plugin.wasm must be a core Wasm module of at most 128 MiB")
			}
			var header [8]byte
			if _, err = io.ReadFull(tr, header[:]); err == nil && !bytes.Equal(header[:], coreHeader) {
				err = errors.New("plugin.wasm must be a portable core module, not a component or native cache")
			}
			if err == nil {
				_, err = output.Write(header[:])
			}
			if err == nil {
				_, err = io.CopyN(output, tr, h.Size-int64(len(header)))
			}
			moduleFound = true
		default:
			if path.Base(member) == "plugin.yaml" || path.Base(member) == "plugin.wasm" {
				return packageMetadata{}, errors.New("plugin metadata and module must be at the package root")
			}
			_, err = io.CopyN(io.Discard, tr, h.Size)
		}
		if err != nil {
			return packageMetadata{}, err
		}
		end = counter.n + (512-h.Size%512)%512
	}
	// Consume zero tar padding and the gzip checksum, rejecting concatenated tar
	// archives and nonzero data hidden after the archive's end markers.
	buffer := make([]byte, 32*1024)
	for {
		n, err := counter.Read(buffer)
		if !allZero(buffer[:n]) {
			return packageMetadata{}, errors.New("plugin archive contains nonzero tar trailer")
		}
		if expandedLimit.N <= 0 {
			return packageMetadata{}, errors.New("plugin archive exceeds 256 MiB expanded")
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return packageMetadata{}, err
		}
	}
	if counter.n%512 != 0 {
		return packageMetadata{}, errors.New("plugin archive has incomplete tar padding")
	}
	if _, err := compressed.ReadByte(); err == nil {
		return packageMetadata{}, errors.New("plugin archive contains trailing gzip content")
	} else if !errors.Is(err, io.EOF) {
		return packageMetadata{}, err
	}
	if compressedLimit.N <= 0 {
		return packageMetadata{}, errors.New("plugin archive exceeds 256 MiB compressed")
	}
	if metadataRaw == nil || !moduleFound {
		return packageMetadata{}, errors.New("plugin archive requires plugin.yaml and plugin.wasm")
	}
	metadata, err := parseMetadata(metadataRaw)
	if err != nil {
		return packageMetadata{}, err
	}
	if metadata.name != root {
		return packageMetadata{}, errors.New("plugin archive root must match metadata name")
	}
	return metadata, nil
}

func parseMetadata(raw []byte) (packageMetadata, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return packageMetadata{}, fmt.Errorf("plugin.yaml: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return packageMetadata{}, errors.New("plugin.yaml must contain exactly one document")
	}
	if err := yamlutil.ValidateNodes(&document); err != nil {
		return packageMetadata{}, err
	}
	if len(document.Content) != 1 {
		return packageMetadata{}, errors.New("plugin.yaml must contain a mapping")
	}
	fields, err := yamlutil.Fields(document.Content[0], "apiVersion", "name", "version", "type", "runtime")
	if err != nil {
		return packageMetadata{}, err
	}
	values := map[string]string{}
	for _, key := range []string{"apiVersion", "name", "version", "type", "runtime"} {
		value, err := yamlutil.Text(fields[key])
		if err != nil {
			return packageMetadata{}, fmt.Errorf("plugin.yaml %s: %w", key, err)
		}
		values[key] = value
	}
	if values["apiVersion"] != "v1" || values["type"] != "dependency/v1" || values["runtime"] != "wasi/preview1" {
		return packageMetadata{}, errors.New("plugin requires apiVersion v1, type dependency/v1 and runtime wasi/preview1")
	}
	metadata := packageMetadata{name: values["name"], version: values["version"]}
	if !safeName(metadata.name) || !validVersion(metadata.version) || len(metadata.title()) > 255 {
		return packageMetadata{}, errors.New("plugin requires a safe name and SemVer version")
	}
	return metadata, nil
}

func safeName(value string) bool {
	if value == "" || !asciiAlnum(value[0]) {
		return false
	}
	for i := range len(value) {
		if !asciiAlnum(value[i]) && value[i] != '-' && value[i] != '_' {
			return false
		}
	}
	return true
}

func validVersion(value string) bool {
	version, build, hasBuild := strings.Cut(value, "+")
	if hasBuild && !identifiers(build, false) {
		return false
	}
	version, pre, hasPre := strings.Cut(version, "-")
	if hasPre && !identifiers(pre, true) {
		return false
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if !numeric(part) || len(part) > 1 && part[0] == '0' {
			return false
		}
	}
	return true
}

func identifiers(value string, prerelease bool) bool {
	for _, part := range strings.Split(value, ".") {
		if part == "" || prerelease && numeric(part) && len(part) > 1 && part[0] == '0' {
			return false
		}
		for i := range len(part) {
			if !asciiAlnum(part[i]) && part[i] != '-' {
				return false
			}
		}
	}
	return true
}

func asciiAlnum(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
}
func numeric(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
func allZero(raw []byte) bool {
	for _, value := range raw {
		if value != 0 {
			return false
		}
	}
	return true
}

type archiveCounter struct {
	reader io.Reader
	n      int64
}

func (r *archiveCounter) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.n += int64(n)
	return n, err
}
