package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/strio-dev/bale"
)

const deliveryHelp = `Export and update publication paths for captured bundles

Usage:
  bale delivery [command]

Available Commands:
  export      Export captured delivery paths as YAML
  update      Carry delivery paths onto a refreshed bundle

Options:
  -h, --help  Help for delivery

Use "bale delivery [command] --help" for more information.`

const deliveryUpdateHelp = `Carry explicit paths from an old delivery file onto a refreshed bundle

Usage:
  bale delivery update [options] BEFORE AFTER

Options:
  -f, --file FILE         Existing delivery YAML bound to BEFORE; - reads stdin (required)
  -o, --output FILE       New delivery YAML; must not exist (required)
      --store PATH        Override the managed local store
      --registry          Interpret both operands as registry references
      --layout            Interpret both operands as explicit OCI layout directories
      --format FORMAT     Output JSON or a Go template (default: readable report)
      --authfile FILE     Registry authentication file
      --plain-http HOST   Permit HTTP for this exact registry host (repeatable)
  -h, --help              Help for delivery update

Examples:
  bale delivery update release:v1 release:v2 -f bale.delivery.yaml -o refreshed.delivery.yaml
  bale delivery update --registry registry.example.com/bundles:v1 registry.example.com/bundles:v2 -f old.yaml -o new.yaml

Preserves exact identities and follows one-to-one captured selector changes.
New artifacts receive defaults; removals and ambiguous paths are reported.
Conflicts return status 125 and write no delivery file. Existing files are never overwritten.
Reads captured metadata without refreshing sources, running plugins or reading opaque payloads.
Registry references require an explicit host and tag or digest.`

func runDelivery(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 1 || (len(args) == 2 && (args[1] == "--help" || args[1] == "-h")) {
		if _, err := fmt.Fprintln(out, deliveryHelp); err != nil {
			return fail(errOut, err)
		}
		return 0
	}
	if args[1] == "export" {
		return runDeliveryExport(ctx, args, out, errOut)
	}
	if args[1] != "update" {
		return fail(errOut, fmt.Errorf("unsupported command; see bale delivery --help"))
	}
	opts, err := parseCommand("delivery update", args[2:])
	if err != nil {
		return fail(errOut, fmt.Errorf("%w\nSee 'bale delivery update --help'", err))
	}
	if opts.help {
		if _, err := fmt.Fprintln(out, deliveryUpdateHelp); err != nil {
			return fail(errOut, err)
		}
		return 0
	}
	if len(opts.args) != 2 || opts.file == "" || opts.outputDelivery == "" {
		return fail(errOut, fmt.Errorf("two bundles, --file and --output are required\nSee 'bale delivery update --help'"))
	}
	if err := validateHTTP(opts.plainHTTP); err != nil {
		return fail(errOut, err)
	}
	if opts.registry {
		for _, ref := range opts.args {
			if err := explicitReference(ref, false); err != nil {
				return fail(errOut, fmt.Errorf("%q: %w", ref, err))
			}
		}
	}
	formatter, err := newOutputFormatter(opts.format, false)
	if err != nil {
		return fail(errOut, err)
	}
	data, err := readInputFile(opts.file)
	if err != nil {
		return fail(errOut, err)
	}
	file, err := bale.DecodeDeliveryFile(data)
	if err != nil {
		return fail(errOut, err)
	}
	output, err := prepareDeliveryOutput(opts.outputDelivery)
	if err != nil {
		return fail(errOut, err)
	}
	defer output.close()
	var result bale.DeliveryUpdateResult
	if !opts.layout && !opts.registry {
		local, openErr := openManaged(ctx, opts, nil)
		if openErr != nil {
			return fail(errOut, openErr)
		}
		result, err = local.UpdateDelivery(ctx, opts.args[0], opts.args[1], file)
	} else {
		client, openErr := newCLIClient(opts, nil)
		if openErr != nil {
			return fail(errOut, openErr)
		}
		if opts.layout {
			result, err = client.UpdateDeliveryLayouts(ctx, opts.args[0], opts.args[1], file)
		} else {
			result, err = client.UpdateDelivery(ctx, opts.args[0], opts.args[1], file)
		}
	}
	if err != nil && !errors.Is(err, bale.ErrConflict) {
		return fail(errOut, err)
	}
	// Render before creating the result file, so bad templates leave no output.
	var rendered bytes.Buffer
	var renderErr error
	if formatter.json || formatter.template != nil {
		renderErr = formatter.writeResult(&rendered, result, "")
	} else {
		renderErr = writeDeliveryUpdateReport(&rendered, result, opts.outputDelivery)
	}
	if renderErr != nil {
		return fail(errOut, errors.Join(err, renderErr))
	}
	if err != nil {
		_, outputErr := rendered.WriteTo(out)
		return fail(errOut, errors.Join(err, outputErr))
	}
	if result.Delivery == nil {
		return fail(errOut, fmt.Errorf("delivery update returned no candidate file"))
	}
	if err := ctx.Err(); err != nil {
		return fail(errOut, err)
	}
	if err := output.write(*result.Delivery); err != nil {
		return fail(errOut, err)
	}
	if _, err := rendered.WriteTo(out); err != nil {
		return fail(errOut, err)
	}
	return 0
}

func writeDeliveryUpdateReport(out io.Writer, result bale.DeliveryUpdateResult, output string) error {
	var buffer bytes.Buffer
	fmt.Fprintf(&buffer, "Before: %s\nAfter:  %s\n", result.Before.Digest, result.After.Digest)
	if len(result.Conflicts) == 0 {
		fmt.Fprintf(&buffer, "Delivery file: %s\n", strconv.Quote(output))
	} else {
		fmt.Fprintf(&buffer, "Conflicts: %d; no delivery file written.\n", len(result.Conflicts))
	}
	if len(result.Changes) > 0 {
		table := tabwriter.NewWriter(&buffer, 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(table, "\nCHANGE\tBEFORE\tAFTER\tPATH"); err != nil {
			return err
		}
		for _, change := range result.Changes {
			if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", change.Change, updateColumn(change.Before), updateColumn(change.After), updateColumn(change.Path)); err != nil {
				return err
			}
		}
		if err := table.Flush(); err != nil {
			return err
		}
	}
	for _, conflict := range result.Conflicts {
		fmt.Fprintf(&buffer, "\nConflict: %s\n", strconv.Quote(conflict.Reason))
		for _, source := range conflict.Sources {
			fmt.Fprintf(&buffer, "  source: %s (path %s)\n", source.Ref, strconv.Quote(source.Path))
		}
		fmt.Fprintf(&buffer, "  targets: %s\n", strings.Join(conflict.Targets, ", "))
		for _, selector := range conflict.Selectors {
			fmt.Fprintf(&buffer, "  selector: %s\n", strconv.Quote(selector))
		}
	}
	_, err := buffer.WriteTo(out)
	return err
}

func updateColumn(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
