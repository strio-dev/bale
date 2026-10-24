package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/store"
)

func managedCommand(command string, opts commandOptions) bool {
	switch command {
	case "ls", "info", "tag", "rm", "prune":
		return true
	case "create", "inspect", "diff":
		return !opts.layout && !opts.registry
	case "pull", "push", "save", "load":
		return !opts.layout
	}
	return false
}

func openManaged(ctx context.Context, opts commandOptions, progress func(bale.Progress)) (*store.Store, error) {
	client, err := bale.NewClient(bale.ClientOptions{
		PlainHTTPRegistries: opts.plainHTTP,
		Credentials:         lazyDockerCredentials(),
		OnProgress:          progress,
		PluginRunner:        configuredPluginRunner(opts),
	})
	if err != nil {
		return nil, err
	}
	return store.OpenContext(ctx, client, store.Options{Path: opts.storePath})
}

func runManaged(ctx context.Context, command string, opts commandOptions, out, errOut io.Writer) int {
	valid := false
	switch command {
	case "create":
		valid = len(opts.args) == 0 && opts.file != ""
	case "pull":
		valid = len(opts.args) == 1
	case "push", "diff":
		valid = len(opts.args) == 2
	case "inspect", "rm":
		valid = len(opts.args) > 0
	case "tag":
		valid = len(opts.args) >= 2
	case "save":
		valid = len(opts.args) == 1 && opts.archiveOutput != ""
	case "load":
		valid = len(opts.args) == 0 && opts.archiveInput != ""
	case "ls", "info", "prune":
		valid = len(opts.args) == 0
	}
	if !valid {
		return fail(errOut, fmt.Errorf("invalid arguments\nSee 'bale %s --help'", command))
	}
	if command == "pull" {
		if err := explicitReference(opts.args[0], false); err != nil {
			return fail(errOut, err)
		}
		if opts.mode != "locked" && opts.mode != "latest" {
			return fail(errOut, fmt.Errorf("--mode must be locked or latest"))
		}
		if opts.allowPlugins && opts.mode != "latest" {
			return fail(errOut, fmt.Errorf("--allow-plugins requires --mode latest"))
		}
	}
	if command == "push" {
		if err := explicitReference(opts.args[1], true); err != nil {
			return fail(errOut, err)
		}
	}
	if command == "inspect" && opts.outputDelivery != "" && (len(opts.args) != 1 || opts.formatSet) {
		return fail(errOut, fmt.Errorf("--output-delivery requires one bundle and cannot be combined with --format"))
	}
	formatter, err := newOutputFormatter(opts.format, command == "inspect")
	if err != nil {
		return fail(errOut, err)
	}
	maps, err := parseMaps(opts.maps)
	if err != nil {
		return fail(errOut, err)
	}
	var recipe bale.Recipe
	if command == "create" {
		raw, err := os.ReadFile(opts.file)
		if err != nil {
			return fail(errOut, err)
		}
		recipe, err = bale.ParseRecipe(raw)
		if err != nil {
			return fail(errOut, err)
		}
	}
	var delivery *bale.DeliveryFile
	if opts.delivery != "" {
		raw, err := os.ReadFile(opts.delivery)
		if err != nil {
			return fail(errOut, err)
		}
		decoded, err := bale.DecodeDeliveryFile(raw)
		if err != nil {
			return fail(errOut, err)
		}
		delivery = &decoded
	}
	var deliveryOutput *deliveryOutputFile
	if opts.outputDelivery != "" {
		deliveryOutput, err = prepareDeliveryOutput(opts.outputDelivery)
		if err != nil {
			return fail(errOut, err)
		}
		defer deliveryOutput.close()
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
	local, err := openManaged(ctx, opts, progress)
	if err != nil {
		return fail(errOut, err)
	}
	var result any
	var resultDigest string
	switch command {
	case "create":
		created, createErr := local.Create(ctx, recipe, store.CreateOptions{Tags: opts.tags, DryRun: opts.dryRun, ExportDelivery: deliveryOutput != nil})
		result, resultDigest, err = created, created.Descriptor.Digest.String(), createErr
		if err == nil && deliveryOutput != nil {
			if created.Delivery == nil {
				err = fmt.Errorf("create returned no delivery inventory")
			} else {
				err = deliveryOutput.write(*created.Delivery)
			}
		}
	case "pull":
		mode := bale.PullLocked
		if opts.mode == "latest" {
			mode = bale.PullLatest
		}
		pulled, pullErr := local.Pull(ctx, opts.args[0], store.PullOptions{Tags: opts.tags, Mode: mode, AllowPlugins: opts.allowPlugins})
		result, resultDigest, err = pulled, pulled.Descriptor.Digest.String(), pullErr
	case "push":
		options := bale.PushOptions{RepositoryMap: maps, Delivery: delivery}
		if opts.dryRun {
			plan, planErr := local.PlanPush(ctx, opts.args[0], opts.args[1], options)
			result, resultDigest, err = plan, plan.Descriptor.Digest.String(), planErr
		} else {
			pushed, pushErr := local.Push(ctx, opts.args[0], opts.args[1], options)
			result, resultDigest, err = pushed, pushed.Descriptor.Digest.String(), pushErr
		}
	case "save":
		saved, saveErr := local.Save(ctx, opts.args[0], opts.archiveOutput)
		result, resultDigest, err = saved, saved.Descriptor.Digest.String(), saveErr
	case "load":
		loaded, loadErr := local.Load(ctx, opts.archiveInput, opts.tags)
		result, resultDigest, err = loaded, loaded.Descriptor.Digest.String(), loadErr
	case "inspect":
		if deliveryOutput != nil {
			file, exportErr := local.ExportDelivery(ctx, opts.args[0])
			if exportErr == nil {
				exportErr = deliveryOutput.write(file)
			}
			if exportErr != nil {
				return fail(errOut, exportErr)
			}
			return 0
		}
		results := make([]bale.InspectResult, 0, len(opts.args))
		var failures []error
		for _, ref := range opts.args {
			inspected, inspectErr := local.Inspect(ctx, ref)
			if inspectErr != nil {
				failures = append(failures, fmt.Errorf("%s: %w", ref, inspectErr))
			} else {
				results = append(results, inspected)
			}
		}
		failures = append(failures, formatter.writeInspections(out, results))
		if err := errors.Join(failures...); err != nil {
			return fail(errOut, err)
		}
		return 0
	case "diff":
		diff, diffErr := local.Diff(ctx, opts.args[0], opts.args[1])
		if diffErr != nil {
			return fail(errOut, diffErr)
		}
		if formatter.json || formatter.template != nil {
			err = formatter.writeResult(out, diff, "")
		} else {
			err = writeDiff(out, diff)
		}
		if err != nil {
			return fail(errOut, err)
		}
		return 0
	case "ls":
		entries, listErr := local.List(ctx)
		if listErr != nil {
			return fail(errOut, listErr)
		}
		if formatter.json {
			err = writeJSON(out, entries)
		} else {
			var buf bytes.Buffer
			if formatter.template != nil {
				for _, entry := range entries {
					if err = formatter.writeResult(&buf, entry, ""); err != nil {
						return fail(errOut, err)
					}
				}
			} else {
				table := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
				if _, err := fmt.Fprintln(table, "NAMES\tDIGEST\tCONTENT"); err != nil {
					return fail(errOut, err)
				}
				for _, entry := range entries {
					kind := "complete"
					if entry.MetadataOnly {
						kind = "metadata"
					}
					names := strings.Join(entry.Names, ", ")
					if names == "" {
						names = "<none>"
					}
					if _, err := fmt.Fprintf(table, "%s\t%s\t%s\n", names, entry.Descriptor.Digest, kind); err != nil {
						return fail(errOut, err)
					}
				}
				if err = table.Flush(); err != nil {
					return fail(errOut, err)
				}
			}
			_, err = buf.WriteTo(out)
		}
	case "tag":
		err = local.Tag(ctx, opts.args[0], opts.args[1:])
	case "rm":
		err = local.Remove(ctx, opts.args)
	case "prune":
		pruned, pruneErr := local.Prune(ctx, opts.dryRun)
		err = pruneErr
		if err == nil {
			if formatter.json || formatter.template != nil {
				err = formatter.writeResult(out, pruned, "")
			} else {
				verb := "Removed"
				if opts.dryRun {
					verb = "Would remove"
				}
				_, err = fmt.Fprintf(out, "%s %d unreferenced blobs (%d bytes) and %d capture records.\n", verb, pruned.Blobs, pruned.Bytes, pruned.Captures)
			}
		}
	case "info":
		if formatter.json || formatter.template != nil {
			err = formatter.writeResult(out, struct {
				Store string `json:"store"`
			}{local.Path()}, "")
		} else {
			_, err = fmt.Fprintf(out, "Store: %s\n", local.Path())
		}
	}
	if err != nil || progressErr != nil {
		var outputErr error
		if formatter.json && !opts.dryRun && resultDigest != "" && (command == "create" || command == "pull" || command == "push") {
			outputErr = formatter.writeResult(out, result, resultDigest)
		}
		return fail(errOut, errors.Join(err, progressErr, outputErr))
	}
	if command == "ls" || command == "tag" || command == "rm" || command == "prune" || command == "info" {
		return 0
	}
	if command == "create" && opts.dryRun && !opts.formatSet {
		err = writeCreatePreview(out, result.(bale.CreateResult))
	} else if command == "push" && opts.dryRun && !opts.formatSet {
		err = writePushPreview(out, result.(bale.DeliveryPlan))
	} else {
		err = formatter.writeResult(out, result, resultDigest)
	}
	if err != nil {
		return fail(errOut, err)
	}
	return 0
}
