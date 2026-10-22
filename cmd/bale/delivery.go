package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/strio-dev/bale"
)

// Reserve a writable sibling before capture; publish it using a no-overwrite
// link only after the complete encoded inventory has been written and closed.
type deliveryOutputFile struct {
	path      string
	temporary string
	file      *os.File
}

func prepareDeliveryOutput(path string) (*deliveryOutputFile, error) {
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("delivery output %q already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".bale-delivery-*")
	if err != nil {
		return nil, err
	}
	return &deliveryOutputFile{path: path, temporary: file.Name(), file: file}, nil
}

func (output *deliveryOutputFile) close() {
	if output.file != nil {
		_ = output.file.Close()
	}
	_ = os.Remove(output.temporary)
}

func (output *deliveryOutputFile) write(delivery bale.DeliveryFile) error {
	data, err := bale.EncodeDeliveryFile(delivery)
	if err != nil {
		return err
	}
	if _, err := output.file.Write(data); err != nil {
		return err
	}
	if err := output.file.Sync(); err != nil {
		return err
	}
	if err := output.file.Close(); err != nil {
		return err
	}
	output.file = nil
	if err := os.Link(output.temporary, output.path); err != nil {
		return fmt.Errorf("write delivery output %q: %w", output.path, err)
	}
	return nil
}
