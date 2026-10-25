package main

import (
	"fmt"
	"io"
	"os"
)

func readInputFile(path string) ([]byte, error) {
	return readInputFileLimit(path, -1)
}

func readInputFileLimit(path string, limit int64) ([]byte, error) {
	var input io.Reader = os.Stdin
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = file.Close() }()
		if limit >= 0 {
			info, err := file.Stat()
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("input must be a regular file or stdin")
			}
			if info.Size() > limit {
				return nil, fmt.Errorf("input exceeds %d bytes", limit)
			}
		}
		input = file
	}
	if limit < 0 {
		return io.ReadAll(input)
	}
	raw, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("input exceeds %d bytes", limit)
	}
	return raw, nil
}
