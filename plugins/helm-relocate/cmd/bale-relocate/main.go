package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	relocate "github.com/strio-dev/bale/plugins/helm-relocate"
)

const help = `Relocate rendered workload images using a Bale destination inspection

Usage:
  bale-relocate --chart REPOSITORY@DIGEST (--records-json JSON | --records-file FILE) [options]

Reads rendered YAML from stdin and writes relocated YAML on success.
Use as a Helm post-renderer through the installed 'bale' plugin.

Options:
      --chart REFERENCE       Exact delivered chart repository@digest (required)
      --records-json JSON     Destination inspection JSON (max 64 KiB)
      --records-file FILE     Destination inspection JSON file (max 4 MiB)
      --bundle REFERENCE      Select a pinned delivered bundle
      --source REFERENCE      Select a pinned original chart
      --capture DIGEST        Select an owning capture
      --declaration DIGEST    Select a declaration
      --context CONTEXT       Select a declaration context
  -h, --help                  Show help

Selections must identify one local record and one selected resolution.
The chart reference is supplied by the caller; Helm does not attest its origin.
`

func run(args []string, in io.Reader, out io.Writer) error {
	options, records, err := relocate.ParseArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		_, err = io.WriteString(out, help)
		return err
	}
	if err != nil {
		return err
	}
	input, err := io.ReadAll(io.LimitReader(in, relocate.MaxManifestBytes+1))
	if err != nil {
		return fmt.Errorf("read manifests: %w", err)
	}
	output, err := relocate.Relocate(input, records, options)
	if err != nil {
		return err
	}
	n, err := out.Write(output)
	if err != nil {
		return fmt.Errorf("write manifests: %w", err)
	}
	if n != len(output) {
		return io.ErrShortWrite
	}
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "bale-relocate: %v\n", err)
		os.Exit(1)
	}
}
