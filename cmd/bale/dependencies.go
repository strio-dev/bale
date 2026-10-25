package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/strio-dev/bale"
)

const dependenciesHelp = `Generate, attach, list, and inspect generic OCI dependency declarations

Usage:
  bale dependencies [command]

Available Commands:
  generate    Run a Wasm producer and output dependency declaration YAML
  attach      Attach a dependency declaration to an existing artifact
  list (ls)   List dependency declaration descriptors for an artifact
  inspect     Inspect dependency declaration payloads for an artifact

Options:
  -h, --help  Help for dependencies

Use "bale dependencies [command] --help" for more information.`

func runDependencies(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 1 || (len(args) == 2 && (args[1] == "--help" || args[1] == "-h")) {
		if _, err := fmt.Fprintln(out, dependenciesHelp); err != nil {
			return fail(errOut, err)
		}
		return 0
	}
	action := args[1]
	if action == "ls" {
		action = "list"
	}
	command := "dependencies " + action
	if action != "attach" && action != "list" && action != "inspect" && action != "generate" {
		return fail(errOut, fmt.Errorf("unsupported command; see bale dependencies --help"))
	}
	opts, err := parseCommand(command, args[2:])
	if err != nil {
		return fail(errOut, fmt.Errorf("%w\nSee 'bale %s --help'", err, command))
	}
	if opts.help {
		if _, err := fmt.Fprintln(out, dependencyCommandHelp(action)); err != nil {
			return fail(errOut, err)
		}
		return 0
	}
	if action == "generate" {
		return runDependencyGeneration(ctx, opts, out, errOut)
	}
	if len(opts.args) != 1 {
		return fail(errOut, fmt.Errorf("invalid arguments\nSee 'bale %s --help'", command))
	}
	if err := validateHTTP(opts.plainHTTP); err != nil {
		return fail(errOut, err)
	}
	if err := explicitReference(opts.args[0], false); err != nil {
		return fail(errOut, err)
	}
	formatter, err := newOutputFormatter(opts.format, action != "attach")
	if err != nil {
		return fail(errOut, err)
	}
	var declaration bale.DependencyDeclaration
	if action == "attach" {
		declaration, err = readDependencySource(ctx, opts)
		if err != nil {
			return fail(errOut, err)
		}
	}
	var progressErr error
	var progress func(bale.Progress)
	if action == "attach" && !opts.quiet {
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
	if action == "inspect" {
		declarations, err := client.InspectDependencies(ctx, opts.args[0])
		if err != nil {
			return fail(errOut, err)
		}
		if err := writeDependencyResults(out, formatter, declarations); err != nil {
			return fail(errOut, err)
		}
		return 0
	}
	if action == "list" {
		descriptors, err := client.ListReferrers(ctx, opts.args[0], bale.ListReferrersOptions{ArtifactType: bale.DependencyArtifactType})
		if err != nil {
			return fail(errOut, err)
		}
		if err := writeDependencyResults(out, formatter, descriptors); err != nil {
			return fail(errOut, err)
		}
		return 0
	}
	subject, err := client.Resolve(ctx, opts.args[0])
	if err != nil {
		return fail(errOut, errors.Join(err, progressErr))
	}
	request, err := bale.PrepareDependencyDeclaration(ctx, subject, declaration)
	if err != nil {
		return fail(errOut, errors.Join(err, progressErr))
	}
	result, err := client.Attach(ctx, request)
	if err != nil {
		return fail(errOut, errors.Join(err, progressErr))
	}
	if err := errors.Join(formatter.writeResult(out, result, result.Descriptor.Digest.String()), progressErr); err != nil {
		return fail(errOut, err)
	}
	return 0
}

func writeDependencyResults[T any](out io.Writer, formatter *outputFormatter, results []T) error {
	if results == nil {
		results = []T{}
	}
	if formatter.json {
		return writeJSON(out, results)
	}
	var buf bytes.Buffer
	for _, result := range results {
		if err := formatter.template.Execute(&buf, result); err != nil {
			return err
		}
	}
	_, err := buf.WriteTo(out)
	return err
}

func dependencyCommandHelp(command string) string {
	if command == "generate" {
		return "Run an OCI WASIp1 producer and write validated dependency declaration YAML.\n\nUsage:\n  bale dependencies generate [options]\n\nOptions:\n" + dependencyPluginOptions + "\n      --authfile FILE     Registry authentication file\n      --plain-http HOST   Permit HTTP for this exact registry host (repeatable)\n  -h, --help              Help for dependencies generate\n\nExamples:\n  bale dependencies generate --plugin registry.example.com/plugins/helm:0.1 --input request.json > dependencies.yaml"
	}
	description := "Attach a YAML dependency declaration as an OCI referrer. Only the subject is resolved; dependency references remain selectors."
	usage := "bale dependencies attach [options] SUBJECT"
	options := "  -f, --file FILE         Dependency declaration YAML; - reads stdin (or use --plugin and --input)\n" + dependencyPluginOptions + "\n      --format FORMAT     Output JSON or a Go template (default: attachment digest)\n  -q, --quiet             Suppress progress output"
	example := "bale dependencies attach registry.example.com/charts/web:1.4 -f dependencies.yaml"
	if command == "list" {
		description = "List verified dependency declaration descriptors for an artifact."
		usage = "bale dependencies list [options] SUBJECT"
		options = "  -f, --format FORMAT     Output JSON or a Go template per descriptor (default: json)"
		example = "bale dependencies list registry.example.com/charts/web:1.4 -f '{{.Digest}}'"
	}
	if command == "inspect" {
		description = "Inspect all verified attached dependency declarations, including their contexts and dependency references. Dependency references are not resolved."
		usage = "bale dependencies inspect [options] SUBJECT"
		options = "  -f, --format FORMAT     Output JSON or a Go template per declaration (default: json)"
		example = "bale dependencies inspect registry.example.com/charts/web:1.4 -f '{{.Declaration.Context}} {{json .Declaration.Dependencies}}'"
	}
	return description + "\n\nUsage:\n  " + usage + "\n\nOptions:\n" + options + "\n      --authfile FILE     Registry authentication file\n      --plain-http HOST   Permit HTTP for this exact registry host (repeatable)\n  -h, --help              Help for dependencies " + command + "\n\nExamples:\n  " + example + "\n\nRegistry references require an explicit host and tag or digest."
}
