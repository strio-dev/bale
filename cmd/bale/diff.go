package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
)

func runDiff(ctx context.Context, opts commandOptions, formatter *outputFormatter, out, errOut io.Writer) int {
	if !opts.layout {
		for _, ref := range opts.args {
			if err := explicitReference(ref, false); err != nil {
				return fail(errOut, fmt.Errorf("%q: %w", ref, err))
			}
		}
	}
	client, err := newCLIClient(opts, nil)
	if err != nil {
		return fail(errOut, err)
	}
	var result bale.DiffResult
	if opts.layout {
		result, err = client.DiffLayouts(ctx, opts.args[0], opts.args[1])
	} else {
		result, err = client.Diff(ctx, opts.args[0], opts.args[1])
	}
	if err != nil {
		return fail(errOut, err)
	}
	if formatter.json || formatter.template != nil {
		err = formatter.writeResult(out, result, "")
	} else {
		err = writeDiff(out, result)
	}
	if err != nil {
		return fail(errOut, err)
	}
	return 0
}

func writeDiff(out io.Writer, result bale.DiffResult) error {
	var buffer bytes.Buffer
	fmt.Fprintf(&buffer, "Before: %s\nAfter:  %s\n", result.Before.Digest, result.After.Digest)
	fmt.Fprintf(&buffer, "Bundle changed: %t\nRecipe changed: %t\n", result.Changed, result.RecipeChanged)
	if !result.Changed {
		fmt.Fprintln(&buffer, "Bundles are identical.")
	} else {
		added, removed := 0, 0
		for _, artifact := range result.Artifacts {
			if artifact.Change == "added" {
				added++
			} else {
				removed++
			}
		}
		fmt.Fprintf(&buffer, "Artifacts: %d added, %d removed\nSelector resolutions: %d changed\nDependency observations: %d changed\n",
			added, removed, len(result.Resolutions), len(result.Declarations))
	}
	if len(result.Artifacts) > 0 {
		fmt.Fprintln(&buffer, "\nArtifacts:")
		for _, artifact := range result.Artifacts {
			sign := "+"
			if artifact.Change == "removed" {
				sign = "-"
			}
			fmt.Fprintf(&buffer, "  %s %s@%s\n", sign, artifact.Repository, artifact.Descriptor.Digest)
		}
	}
	if len(result.Resolutions) > 0 {
		fmt.Fprintln(&buffer, "\nSelector resolutions:")
		table := tabwriter.NewWriter(&buffer, 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(table, "KIND\tSELECTOR\tBEFORE\tAFTER"); err != nil {
			return err
		}
		for _, resolution := range result.Resolutions {
			if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", resolution.Kind, resolution.Ref, diffDigests(resolution.Before), diffDigests(resolution.After)); err != nil {
				return err
			}
		}
		if err := table.Flush(); err != nil {
			return err
		}
	}
	if len(result.Declarations) > 0 {
		fmt.Fprintln(&buffer, "\nDependency declarations:")
		for _, declaration := range result.Declarations {
			fmt.Fprintf(&buffer, "  %s\n", declaration.Subject)
			writeDiffStates(&buffer, "before", declaration.Before)
			writeDiffStates(&buffer, "after", declaration.After)
		}
	}
	_, err := buffer.WriteTo(out)
	return err
}

func diffDigests(descriptors []v1.Descriptor) string {
	if len(descriptors) == 0 {
		return "-"
	}
	digests := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		digests = append(digests, descriptor.Digest.String())
	}
	return strings.Join(digests, ", ")
}

func writeDiffStates(buffer *bytes.Buffer, side string, states []bale.DeclarationState) {
	if len(states) == 0 {
		fmt.Fprintf(buffer, "    %s: -\n", side)
	}
	for _, state := range states {
		fmt.Fprintf(buffer, "    %s: %s, follow=%s", side, state.Outcome, state.Policy.Follow)
		if state.Policy.Select != nil {
			if state.Policy.Select.Digest != "" {
				fmt.Fprintf(buffer, ", select-digest=%s", state.Policy.Select.Digest)
			}
			if state.Policy.Select.Context != "" {
				fmt.Fprintf(buffer, ", select-context=%s", strconv.Quote(state.Policy.Select.Context))
			}
		}
		if state.Descriptor != nil {
			fmt.Fprintf(buffer, ", declaration=%s", state.Descriptor.Digest)
		}
		if state.Context != "" {
			fmt.Fprintf(buffer, ", context=%s", strconv.Quote(state.Context))
		}
		fmt.Fprintln(buffer)
	}
}
