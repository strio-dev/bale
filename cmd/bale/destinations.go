package main

import (
	"context"
	"fmt"
	"io"
)

const destinationsHelp = `Inspect destination records attached to delivered OCI artifacts

Usage:
  bale destinations [command]

Available Commands:
  inspect     Inspect verified destination records for an artifact

Options:
  -h, --help  Help for destinations

Use "bale destinations inspect --help" for more information.`

const destinationInspectHelp = `Inspect all verified destination records for an artifact

Usage:
  bale destinations inspect [options] SUBJECT

Options:
  -f, --format FORMAT     Output JSON or a Go template per record (default: json)
      --plain-http HOST   Permit HTTP for this exact registry host (repeatable)
  -h, --help              Help for destinations inspect

Examples:
  bale destinations inspect registry.example.com/release/charts/web:1.4
  bale destinations inspect registry.example.com/release/charts/web:1.4 -f '{{.Local}} {{.Record.Bundle}}'

Local identifies records whose destination matches this repository and digest.
Select the intended source, bundle, capture and declaration; no newest record is chosen.
Reading metadata does not verify current target availability or bundle completion.
Registry references require an explicit host and tag or digest.`

func runDestinations(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 1 || (len(args) == 2 && (args[1] == "--help" || args[1] == "-h")) {
		if _, err := fmt.Fprintln(out, destinationsHelp); err != nil {
			return fail(errOut, err)
		}
		return 0
	}
	if args[1] != "inspect" {
		return fail(errOut, fmt.Errorf("unsupported command; see bale destinations --help"))
	}
	opts, err := parseCommand("destinations inspect", args[2:])
	if err != nil {
		return fail(errOut, fmt.Errorf("%w\nSee 'bale destinations inspect --help'", err))
	}
	if opts.help {
		if _, err := fmt.Fprintln(out, destinationInspectHelp); err != nil {
			return fail(errOut, err)
		}
		return 0
	}
	if len(opts.args) != 1 {
		return fail(errOut, fmt.Errorf("invalid arguments\nSee 'bale destinations inspect --help'"))
	}
	if err := validateHTTP(opts.plainHTTP); err != nil {
		return fail(errOut, err)
	}
	if err := explicitReference(opts.args[0], false); err != nil {
		return fail(errOut, err)
	}
	formatter, err := newOutputFormatter(opts.format, true)
	if err != nil {
		return fail(errOut, err)
	}
	client, err := newCLIClient(opts, nil)
	if err != nil {
		return fail(errOut, err)
	}
	records, err := client.InspectDestinations(ctx, opts.args[0])
	if err != nil {
		return fail(errOut, err)
	}
	if err := writeDependencyResults(out, formatter, records); err != nil {
		return fail(errOut, err)
	}
	return 0
}
