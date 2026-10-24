package wasmtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
)

// Package wraps a portable module in a deterministic discovery package.
func Package(module []byte) []byte {
	const metadata = `apiVersion: v1
name: discovery
version: 0.0.0
type: dependency/v1
runtime: wasi/preview1
`
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tarball := tar.NewWriter(gz)
	for _, file := range []struct {
		name string
		data []byte
	}{
		{"discovery/plugin.yaml", []byte(metadata)},
		{"discovery/plugin.wasm", module},
		{"discovery/LICENSE", []byte("Fixture license notice\n")},
	} {
		if err := tarball.WriteHeader(&tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.data))}); err != nil {
			panic(err)
		}
		if _, err := tarball.Write(file.data); err != nil {
			panic(err)
		}
	}
	if err := tarball.Close(); err != nil {
		panic(err)
	}
	if err := gz.Close(); err != nil {
		panic(err)
	}
	return output.Bytes()
}
