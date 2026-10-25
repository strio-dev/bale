package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/strio-dev/bale"
)

const deliveryExportHelp = `Export a bundle's captured delivery paths as YAML

Usage:
  bale delivery export [options] BUNDLE

Options:
  -o, --output FILE       Write a new delivery YAML file (default: stdout; - uses stdout)
      --store PATH        Override the managed local store
      --registry          Read a registry reference instead of a stored bundle
      --layout            Read an explicit local OCI layout directory
      --authfile FILE     Registry authentication file
      --plain-http HOST   Permit HTTP for this exact registry host (repeatable)
  -h, --help              Help for delivery export

Examples:
  bale delivery export release:v1 -o bale.delivery.yaml
  bale delivery export --registry registry.example.com/bundles/release:v1 > bale.delivery.yaml

Exports captured metadata without refreshing sources or running plugins.
Existing output files are never overwritten.`

func runDeliveryExport(ctx context.Context, args []string, out, errOut io.Writer) int {
	opts, err := parseCommand("delivery export", args[2:])
	if err != nil {
		return fail(errOut, fmt.Errorf("%w\nSee 'bale delivery export --help'", err))
	}
	if opts.help {
		if _, err := fmt.Fprintln(out, deliveryExportHelp); err != nil {
			return fail(errOut, err)
		}
		return 0
	}
	if len(opts.args) != 1 {
		return fail(errOut, fmt.Errorf("requires one bundle\nSee 'bale delivery export --help'"))
	}
	if err := validateHTTP(opts.plainHTTP); err != nil {
		return fail(errOut, err)
	}
	if opts.registry {
		if err := explicitReference(opts.args[0], false); err != nil {
			return fail(errOut, err)
		}
	}
	var output *deliveryOutputFile
	if opts.outputDelivery != "" && opts.outputDelivery != "-" {
		output, err = prepareDeliveryOutput(opts.outputDelivery)
		if err != nil {
			return fail(errOut, err)
		}
		defer output.close()
	}
	var delivery bale.DeliveryFile
	if !opts.layout && !opts.registry {
		local, openErr := openManaged(ctx, opts, nil)
		if openErr != nil {
			return fail(errOut, openErr)
		}
		delivery, err = local.ExportDelivery(ctx, opts.args[0])
	} else {
		client, openErr := newCLIClient(opts, nil)
		if openErr != nil {
			return fail(errOut, openErr)
		}
		if opts.layout {
			delivery, err = client.ExportDeliveryLayout(ctx, opts.args[0])
		} else {
			delivery, err = client.ExportDelivery(ctx, opts.args[0])
		}
	}
	if err != nil {
		return fail(errOut, err)
	}
	if err := ctx.Err(); err != nil {
		return fail(errOut, err)
	}
	if output != nil {
		err = output.write(delivery)
	} else {
		var raw []byte
		raw, err = bale.EncodeDeliveryFile(delivery)
		if err == nil {
			_, err = out.Write(raw)
		}
	}
	if err != nil {
		return fail(errOut, err)
	}
	return 0
}

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
