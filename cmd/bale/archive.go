package main

import (
	"context"
	"fmt"
	"io"

	"github.com/strio-dev/bale"
)

func runArchive(ctx context.Context, command string, opts commandOptions, out, errOut io.Writer) int {
	if len(opts.args) != 1 || (command == "save" && opts.archiveOutput == "") || (command == "load" && opts.archiveInput == "") {
		return fail(errOut, fmt.Errorf("invalid arguments\nSee 'bale %s --help'", command))
	}
	formatter, err := newOutputFormatter(opts.format, false)
	if err != nil {
		return fail(errOut, err)
	}
	// Archive operations are local and must not depend on Docker credentials.
	client, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		return fail(errOut, err)
	}
	var result bale.ArchiveResult
	if command == "save" {
		result, err = client.Save(ctx, opts.args[0], opts.archiveOutput)
	} else {
		result, err = client.Load(ctx, opts.archiveInput, opts.args[0])
	}
	if err != nil {
		return fail(errOut, err)
	}
	if err := formatter.writeResult(out, result, result.Descriptor.Digest.String()); err != nil {
		return fail(errOut, err)
	}
	return 0
}
