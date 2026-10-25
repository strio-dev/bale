package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/plugin"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"
)

const pluginsHelp = `Publish portable OCI discovery plugins

Usage:
  bale plugins [command]

Available Commands:
  push       Publish a packaged discovery plugin to a registry

Options:
  -h, --help  Help for plugins

Use "bale plugins push --help" for more information.`

const pluginPushHelp = `Publish a packaged discovery plugin as an OCI artifact.
The archive includes plugin metadata, its WASIp1 module and license notices.
Package bytes are transferred unchanged; compiled native caches are never published.

Usage:
  bale plugins push [options] PLUGINREF

Options:
  -f, --file FILE         Discovery plugin .tgz archive (required)
      --format FORMAT     Output JSON or a Go template (default: plugin digest)
  -q, --quiet             Suppress progress output
      --plain-http HOST   Permit HTTP for this exact registry host (repeatable)
      --authfile FILE     Registry authentication file
  -h, --help              Help for plugins push

Examples:
  bale plugins push -f helm-discovery-0.1.0.tgz registry.example.com/plugins/helm-discovery:0.1.0`

type pluginPushResult struct {
	Reference  string        `json:"reference"`
	Descriptor v1.Descriptor `json:"descriptor"`
	bale.Result
}

func runPlugins(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 1 || (len(args) == 2 && (args[1] == "--help" || args[1] == "-h")) {
		if _, err := fmt.Fprintln(out, pluginsHelp); err != nil {
			return fail(errOut, err)
		}
		return 0
	}
	if args[1] != "push" {
		return fail(errOut, fmt.Errorf("unsupported command; see bale plugins --help"))
	}
	opts, err := parseCommand("plugins push", args[2:])
	if err != nil {
		return fail(errOut, fmt.Errorf("%w\nSee 'bale plugins push --help'", err))
	}
	if opts.help {
		if _, err := fmt.Fprintln(out, pluginPushHelp); err != nil {
			return fail(errOut, err)
		}
		return 0
	}
	if len(opts.args) != 1 || opts.file == "" {
		return fail(errOut, fmt.Errorf("provide --file and one plugin reference; see 'bale plugins push --help'"))
	}
	if err := explicitReference(opts.args[0], true); err != nil {
		return fail(errOut, err)
	}
	if err := validateHTTP(opts.plainHTTP); err != nil {
		return fail(errOut, err)
	}
	formatter, err := newOutputFormatter(opts.format, false)
	if err != nil {
		return fail(errOut, err)
	}
	// Use disk-backed OCI storage so plugin archives are not retained in a
	// second in-memory buffer while packing and uploading.
	dir, err := os.MkdirTemp("", "bale-plugin-publish-*")
	if err != nil {
		return fail(errOut, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	store, err := oci.New(dir)
	if err != nil {
		return fail(errOut, err)
	}
	descriptor, err := plugin.Pack(ctx, store, opts.file)
	if err != nil {
		return fail(errOut, err)
	}
	var progressErr error
	var progress func(bale.Progress)
	if !opts.quiet {
		progress = func(event bale.Progress) {
			if progressErr == nil {
				progressErr = writeProgress(errOut, event)
			}
		}
	}
	client, err := newCLIClient(opts, progress)
	if err != nil {
		return fail(errOut, err)
	}
	ref, err := registry.ParseReference(opts.args[0])
	if err != nil {
		return fail(errOut, err)
	}
	const source = "local.invalid/plugin"
	plan, err := client.Plan(ctx, bale.TransferRequest{
		Roots: []bale.Root{{Repository: source, Descriptor: descriptor, Store: store,
			Aliases: []string{ref.Reference}, Referrers: bale.ReferrerPolicy{Include: "none"}}},
		Destination: ref.Registry, RepositoryMap: map[string]string{source: ref.Repository},
	})
	if err != nil {
		return fail(errOut, errors.Join(err, progressErr))
	}
	result, err := client.Execute(ctx, plan)
	if err != nil {
		return fail(errOut, errors.Join(err, progressErr))
	}
	response := pluginPushResult{Reference: opts.args[0], Descriptor: descriptor, Result: result}
	if err := errors.Join(formatter.writeResult(out, response, descriptor.Digest.String()), progressErr); err != nil {
		return fail(errOut, err)
	}
	return 0
}
