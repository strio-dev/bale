package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/registry"
)

var version = "dev"

const help = `Bundle OCI images and artifacts for registry and offline transfer

Usage:
  bale [command]

Available Commands:
  create        Resolve a recipe into the managed local store
  delivery      Export or update artifact delivery paths
  dependencies  Generate, attach, list, and inspect dependency declarations
  destinations  Inspect delivered artifact destination records
  diff          Compare captured bundle snapshots
  inspect       Inspect captured bundle content
  load          Import an OCI bundle archive into the local store
  ls            List stored bundles
  info          Show the local store location
  tag           Add names to a stored bundle
  rm            Remove stored bundle names or IDs
  prune         Remove unreferenced local content
  plugins       Publish portable discovery plugins
  pull          Pull a bundle into the local store
  push          Push a local bundle to a registry
  save          Save a local bundle as an OCI archive
  version       Display version information
  help          Help about a command

Options:
      --store PATH  Override the managed local store
  -h, --help       Help for bale
  -v, --version    Display version information

Use "bale [command] --help" for more information about a command.`

type stringsFlag []string

func (v *stringsFlag) String() string     { return strings.Join(*v, ",") }
func (v *stringsFlag) Set(s string) error { *v = append(*v, s); return nil }

type commandOptions struct {
	file, mode, format, storePath, authFile string
	outputDelivery, delivery                string
	archiveInput, archiveOutput             string
	dryRun, help                            bool
	formatSet                               bool
	layout, registry, quiet, noTrunc        bool
	plainHTTP, maps, tags                   stringsFlag
	args                                    []string
	pluginModule, pluginInput, pluginCache  string
	pluginRef                               string
	allowPlugins                            bool
	pluginTimeout                           time.Duration
	pluginMemory                            uint64
	pluginCacheSet, pluginOptionsSet        bool
}

// parseCommand retains standard flag value parsing while allowing flags after operands.
func parseCommand(command string, args []string) (commandOptions, error) {
	var opts commandOptions
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.help, "help", false, "help")
	fs.BoolVar(&opts.help, "h", false, "help")
	allowed := map[string]bool{"help": false, "h": false, "store": true}
	fs.StringVar(&opts.storePath, "store", "", "managed local store directory")
	if registryCommand(command) {
		fs.Var(&opts.plainHTTP, "plain-http", "registry allowed to use HTTP")
		allowed["plain-http"] = true
		fs.StringVar(&opts.authFile, "authfile", "", "registry authentication file")
		allowed["authfile"] = true
	}
	aliases := map[string]string{"h": "help"}
	repeated := map[string]bool{"plain-http": true}
	if command == "create" || command == "pull" || command == "load" {
		fs.Var(&opts.tags, "tag", "local bundle name")
		fs.Var(&opts.tags, "t", "local bundle name")
		allowed["tag"], allowed["t"] = true, true
		aliases["t"] = "tag"
		repeated["tag"] = true
	}
	if command == "inspect" || command == "diff" || command == "delivery update" || command == "delivery export" {
		fs.BoolVar(&opts.registry, "registry", false, "use registry references")
		allowed["registry"] = false
	}
	if command == "pull" || command == "push" || command == "save" || command == "load" {
		fs.BoolVar(&opts.layout, "layout", false, "use an explicit OCI layout path")
		allowed["layout"] = false
	}
	if command != "version" && command != "dependencies generate" && command != "delivery export" && command != "tag" && command != "rm" {
		fs.StringVar(&opts.format, "format", "", "output format")
		allowed["format"] = true
	}
	if command == "create" || command == "pull" || command == "push" || command == "save" || command == "load" || command == "dependencies attach" || command == "plugins push" || command == "ls" {
		fs.BoolVar(&opts.quiet, "quiet", false, "suppress progress")
		fs.BoolVar(&opts.quiet, "q", false, "suppress progress")
		allowed["quiet"], allowed["q"] = false, false
		aliases["q"] = "quiet"
	}
	switch command {
	case "create", "dependencies attach", "plugins push":
		fs.StringVar(&opts.file, "file", "", "recipe file")
		fs.StringVar(&opts.file, "f", "", "recipe file")
		allowed["file"], allowed["f"] = true, true
		aliases["f"] = "file"
		if command != "create" {
			break
		}
		fs.BoolVar(&opts.dryRun, "dry-run", false, "resolve without publication")
		fs.BoolVar(&opts.layout, "layout", false, "download a complete local OCI directory")
		fs.BoolVar(&opts.registry, "registry", false, "capture into a registry repository")
		fs.StringVar(&opts.outputDelivery, "output-delivery", "", "write delivery YAML")
		allowed["output-delivery"] = true
		allowed["dry-run"] = false
		allowed["layout"] = false
		allowed["registry"] = false
	case "save":
		fs.StringVar(&opts.archiveOutput, "output", "", "new OCI tar archive")
		fs.StringVar(&opts.archiveOutput, "o", "", "new OCI tar archive")
		allowed["output"], allowed["o"] = true, true
		aliases["o"] = "output"
	case "load":
		fs.StringVar(&opts.archiveInput, "input", "", "OCI tar archive")
		fs.StringVar(&opts.archiveInput, "i", "", "OCI tar archive")
		allowed["input"], allowed["i"] = true, true
		aliases["i"] = "input"
	case "dependencies list", "dependencies inspect", "destinations inspect":
		fs.StringVar(&opts.format, "f", "", "output format")
		allowed["f"] = true
		aliases["f"] = "format"
	case "inspect":
		fs.StringVar(&opts.format, "f", "", "output format")
		fs.BoolVar(&opts.layout, "layout", false, "inspect local OCI layouts")
		allowed["f"], allowed["layout"] = true, false
		aliases["f"] = "format"
	case "diff":
		fs.StringVar(&opts.format, "f", "", "output format")
		fs.BoolVar(&opts.layout, "layout", false, "compare local OCI layouts")
		allowed["f"], allowed["layout"] = true, false
		aliases["f"] = "format"
	case "delivery export":
		fs.StringVar(&opts.outputDelivery, "output", "", "delivery YAML output (default: stdout)")
		fs.StringVar(&opts.outputDelivery, "o", "", "delivery YAML output (default: stdout)")
		fs.BoolVar(&opts.layout, "layout", false, "read an explicit OCI layout")
		allowed["output"], allowed["o"], allowed["layout"] = true, true, false
		aliases["o"] = "output"
	case "delivery update":
		fs.StringVar(&opts.file, "file", "", "existing delivery YAML")
		fs.StringVar(&opts.file, "f", "", "existing delivery YAML")
		fs.StringVar(&opts.outputDelivery, "output", "", "new delivery YAML")
		fs.StringVar(&opts.outputDelivery, "o", "", "new delivery YAML")
		fs.BoolVar(&opts.layout, "layout", false, "read local OCI layouts")
		allowed["file"], allowed["f"], allowed["output"], allowed["o"], allowed["layout"] = true, true, true, true, false
		aliases["f"], aliases["o"] = "file", "output"
	case "pull":
		fs.StringVar(&opts.mode, "mode", "locked", "pull mode")
		allowed["mode"] = true
		fs.BoolVar(&opts.allowPlugins, "allow-plugins", false, "allow configured plugins during latest refresh")
		allowed["allow-plugins"] = false
	case "push":
		fs.Var(&opts.maps, "map", "source repository mapping")
		fs.StringVar(&opts.delivery, "delivery", "", "delivery YAML file")
		fs.BoolVar(&opts.dryRun, "dry-run", false, "preview local delivery plan without registry access")
		allowed["dry-run"] = false
		allowed["delivery"] = true
		allowed["map"], repeated["map"] = true, true
	case "prune":
		fs.BoolVar(&opts.dryRun, "dry-run", false, "preview unreferenced content removal")
		allowed["dry-run"] = false
	case "ls":
		fs.BoolVar(&opts.noTrunc, "no-trunc", false, "show complete bundle digests")
		allowed["no-trunc"] = false
	case "tag", "rm", "info":
	case "version":
	case "dependencies generate":
	default:
		return opts, fmt.Errorf("unsupported command; see bale --help")
	}
	if command == "dependencies attach" || command == "dependencies generate" {
		fs.StringVar(&opts.pluginRef, "plugin", "", "dependency producer OCI reference")
		fs.StringVar(&opts.pluginModule, "plugin-file", "", "local dependency producer Wasm file")
		fs.StringVar(&opts.pluginInput, "input", "", "plugin request JSON file")
		for _, name := range []string{"plugin", "plugin-file", "input"} {
			allowed[name] = true
		}
	}
	if command == "create" || command == "pull" || command == "dependencies attach" || command == "dependencies generate" {
		fs.StringVar(&opts.pluginCache, "plugin-cache", "", "trusted local compiled cache directory")
		fs.DurationVar(&opts.pluginTimeout, "plugin-timeout", 0, "guest execution timeout")
		fs.Var(pluginMemoryFlag{&opts.pluginMemory}, "plugin-memory", "guest linear memory limit (bytes or KiB/MiB/GiB)")
		for _, name := range []string{"plugin-cache", "plugin-timeout", "plugin-memory"} {
			allowed[name] = true
		}
	}
	var flags []string
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			opts.args = append(opts.args, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			opts.args = append(opts.args, arg)
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		if !strings.HasPrefix(arg, "--") && !hasValue && len(name) > 1 && allowed[name[:1]] {
			arg = "-" + name[:1] + "=" + name[1:]
			name, hasValue = name[:1], true
		}
		needsValue, ok := allowed[name]
		if !ok {
			return opts, fmt.Errorf("unknown option %q", arg)
		}
		key := name
		if canonical, ok := aliases[name]; ok {
			key = canonical
		}
		if seen[key] && !repeated[key] {
			return opts, fmt.Errorf("duplicate option --%s", key)
		}
		seen[key] = true
		flags = append(flags, arg)
		if needsValue && !hasValue {
			if i+1 == len(args) || (strings.HasPrefix(args[i+1], "-") && args[i+1] != "-") {
				return opts, fmt.Errorf("--%s requires a value", name)
			}
			i++
			flags = append(flags, args[i])
		}
	}
	if err := fs.Parse(flags); err != nil {
		return opts, err
	}
	if opts.layout && opts.registry {
		return opts, fmt.Errorf("--layout and --registry cannot be combined")
	}
	if seen["store"] && opts.storePath == "" {
		return opts, fmt.Errorf("--store requires a nonempty path")
	}
	if len(opts.tags) > 0 && (opts.layout || opts.registry) {
		return opts, fmt.Errorf("--tag is for managed local bundles; it cannot be combined with --layout or --registry")
	}
	if seen["output-delivery"] && opts.outputDelivery == "" {
		return opts, fmt.Errorf("--output-delivery requires a nonempty path")
	}
	if seen["output"] && opts.outputDelivery == "" && opts.archiveOutput == "" {
		return opts, fmt.Errorf("--output requires a nonempty path")
	}
	if command == "load" && seen["input"] && opts.archiveInput == "" {
		return opts, fmt.Errorf("--input requires a nonempty path")
	}
	if seen["delivery"] && opts.delivery == "" {
		return opts, fmt.Errorf("--delivery requires a nonempty path")
	}
	if seen["authfile"] && opts.authFile == "" {
		return opts, fmt.Errorf("--authfile requires a nonempty path")
	}
	if command == "save" && (opts.archiveOutput == "" || opts.archiveOutput == "-") && seen["format"] {
		return opts, fmt.Errorf("--format requires --output FILE; stdout is reserved for archive bytes")
	}
	if command == "ls" && seen["format"] && (seen["quiet"] || seen["no-trunc"]) {
		return opts, fmt.Errorf("--format cannot be combined with --quiet or --no-trunc")
	}
	opts.formatSet = seen["format"]
	opts.pluginCacheSet = seen["plugin-cache"]
	opts.pluginOptionsSet = seen["plugin-cache"] || seen["plugin-timeout"] || seen["plugin-memory"] || seen["input"] || seen["plugin"] || seen["plugin-file"]
	return opts, nil
}

// Registry settings are exposed only by operations that can access a registry.
func registryCommand(command string) bool {
	switch command {
	case "create", "pull", "push", "save", "inspect", "diff", "delivery update", "delivery export", "dependencies attach", "dependencies list", "dependencies inspect", "dependencies generate", "destinations inspect", "plugins push":
		return true
	}
	return false
}

func explicitReference(value string, publication bool) error {
	ref, err := registry.ParseReference(value)
	if err != nil {
		return err
	}
	if ref.Registry != "localhost" && !strings.ContainsAny(ref.Registry, ".:") {
		return fmt.Errorf("explicit registry host required")
	}
	if ref.Reference == "" {
		return fmt.Errorf("explicit tag or digest required")
	}
	if publication {
		return ref.ValidateReferenceAsTag()
	}
	return nil
}

func validateHTTP(hosts []string) error {
	for _, host := range hosts {
		ref := registry.Reference{Registry: host}
		if err := ref.ValidateRegistry(); err != nil {
			return err
		}
		if host != "localhost" && !strings.ContainsAny(host, ".:") {
			return fmt.Errorf("--plain-http requires an explicit registry host")
		}
	}
	return nil
}

func parseMaps(values []string) (map[string]string, error) {
	result := map[string]string{}
	for _, value := range values {
		source, target, ok := strings.Cut(value, "=")
		if !ok || source == "" || target == "" {
			return nil, fmt.Errorf("--map requires SOURCE_REPO=RELATIVE_PATH")
		}
		ref, err := registry.ParseReference(source)
		if err != nil || ref.Reference != "" || (ref.Registry != "localhost" && !strings.ContainsAny(ref.Registry, ".:")) {
			return nil, fmt.Errorf("invalid map source %q", source)
		}
		if err := (registry.Reference{Repository: target}).ValidateRepository(); err != nil {
			return nil, fmt.Errorf("invalid map path %q", target)
		}
		if _, exists := result[source]; exists {
			return nil, fmt.Errorf("duplicate map source %q", source)
		}
		result[source] = target
	}
	return result, nil
}

func fail(errOut io.Writer, err error) int {
	_, _ = fmt.Fprintln(errOut, "Error: "+err.Error())
	return 125
}

func run(args []string, out, errOut io.Writer) (code int) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	input, closeInput, err := commandPipe(ctx, os.Stdin)
	if err != nil {
		return fail(errOut, err)
	}
	originalInput := os.Stdin
	os.Stdin = input
	defer func() {
		os.Stdin = originalInput
		if err := closeInput(); err != nil {
			code = fail(errOut, err)
		}
	}()
	output, closeOutput, err := commandOutput(ctx, out)
	if err != nil {
		return fail(errOut, err)
	}
	defer func() {
		if err := closeOutput(); err != nil {
			code = fail(errOut, err)
		}
	}()
	return runContext(ctx, args, output, errOut)
}

func runContext(ctx context.Context, args []string, out, errOut io.Writer) int {
	var global []string
	for len(args) > 0 && (args[0] == "--store" || strings.HasPrefix(args[0], "--store=")) {
		if args[0] == "--store" {
			if len(args) < 2 || strings.HasPrefix(args[1], "-") {
				return fail(errOut, fmt.Errorf("--store requires a value"))
			}
			global = append(global, "--store="+args[1])
			args = args[2:]
		} else {
			global = append(global, args[0])
			args = args[1:]
		}
	}
	if len(args) > 0 && args[0] != "help" && args[0] != "--help" && args[0] != "-h" && args[0] != "--version" && args[0] != "-v" && args[0] != "version" {
		position := len(args)
		for i, arg := range args {
			if arg == "--" {
				position = i
				break
			}
		}
		prefix := append([]string{}, args[:position]...)
		prefix = append(prefix, global...)
		args = append(prefix, args[position:]...)
	}
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")) {
		if _, err := fmt.Fprintln(out, help); err != nil {
			return fail(errOut, err)
		}
		return 0
	}
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v" || args[0] == "version") {
		if _, err := fmt.Fprintln(out, "bale "+version); err != nil {
			return fail(errOut, err)
		}
		return 0
	}
	if args[0] == "help" && len(args) >= 2 && args[1] == "dependencies" {
		return runDependencies(ctx, append(args[1:], "--help"), out, errOut)
	}
	if args[0] == "help" && len(args) >= 2 && args[1] == "delivery" {
		return runDelivery(ctx, append(args[1:], "--help"), out, errOut)
	}
	if args[0] == "delivery" {
		return runDelivery(ctx, args, out, errOut)
	}
	if args[0] == "dependencies" {
		return runDependencies(ctx, args, out, errOut)
	}
	if args[0] == "help" && len(args) >= 2 && args[1] == "destinations" {
		return runDestinations(ctx, append(args[1:], "--help"), out, errOut)
	}
	if args[0] == "destinations" {
		return runDestinations(ctx, args, out, errOut)
	}
	if args[0] == "help" && len(args) >= 2 && args[1] == "plugins" {
		return runPlugins(ctx, append(args[1:], "--help"), out, errOut)
	}
	if args[0] == "plugins" {
		return runPlugins(ctx, args, out, errOut)
	}
	if args[0] == "help" && len(args) == 2 {
		args = []string{args[1], "--help"}
	}
	opts, err := parseCommand(args[0], args[1:])
	if err != nil {
		return fail(errOut, fmt.Errorf("%w\nSee 'bale %s --help'", err, args[0]))
	}
	if opts.help {
		if _, err := fmt.Fprintln(out, commandHelp(args[0])); err != nil {
			return fail(errOut, err)
		}
		return 0
	}
	if err := validateHTTP(opts.plainHTTP); err != nil {
		return fail(errOut, err)
	}
	if managedCommand(args[0], opts) {
		return runManaged(ctx, args[0], opts, out, errOut)
	}
	if args[0] == "save" || args[0] == "load" {
		return runArchive(ctx, args[0], opts, out, errOut)
	}
	maps, err := parseMaps(opts.maps)
	if err != nil {
		return fail(errOut, err)
	}
	command := args[0]
	validArgs := (command == "create" && len(opts.args) == 1 && opts.file != "") ||
		(command == "inspect" && len(opts.args) > 0) ||
		((command == "pull" || command == "push" || command == "diff") && len(opts.args) == 2)
	if !validArgs {
		return fail(errOut, fmt.Errorf("invalid arguments\nSee 'bale %s --help'", command))
	}
	var delivery *bale.DeliveryFile
	if opts.delivery != "" {
		data, err := readInputFile(opts.delivery)
		if err != nil {
			return fail(errOut, err)
		}
		decoded, err := bale.DecodeDeliveryFile(data)
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
	formatter, err := newOutputFormatter(opts.format, command == "inspect")
	if err != nil {
		return fail(errOut, err)
	}
	if command == "diff" {
		return runDiff(ctx, opts, formatter, out, errOut)
	}
	if command == "inspect" {
		return runInspect(ctx, opts, formatter, out, errOut)
	}
	ref := opts.args[0]
	if command == "push" {
		ref = opts.args[1]
	}
	if command != "create" || opts.registry {
		if err := explicitReference(ref, command != "pull"); err != nil {
			return fail(errOut, err)
		}
	}
	if command == "pull" && opts.mode != "locked" && opts.mode != "latest" {
		return fail(errOut, fmt.Errorf("--mode must be locked or latest"))
	}
	if command == "pull" && opts.allowPlugins && opts.mode != "latest" {
		return fail(errOut, fmt.Errorf("--allow-plugins requires --mode latest"))
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
	var service *bale.Client
	if command == "push" && opts.dryRun {
		// Planning is entirely local and must not depend on ambient credentials.
		service, err = bale.NewClient(bale.ClientOptions{})
	} else {
		service, err = newCLIClient(opts, progress)
	}
	if err != nil {
		return fail(errOut, err)
	}
	var result any
	var resultDigest string
	switch command {
	case "create":
		var data []byte
		data, err = readInputFile(opts.file)
		if err == nil {
			var recipe bale.Recipe
			recipe, err = bale.ParseRecipe(data)
			if err == nil {
				var created bale.CreateResult
				createOptions := bale.CreateOptions{DryRun: opts.dryRun, ExportDelivery: deliveryOutput != nil}
				switch {
				case opts.registry:
					created, err = service.Create(ctx, recipe, ref, createOptions)
				case opts.layout:
					created, err = service.CreateLayout(ctx, recipe, ref, createOptions)
				default:
					created, err = service.CreateLocal(ctx, recipe, ref, createOptions)
				}
				result, resultDigest = created, created.Descriptor.Digest.String()
				if err == nil && deliveryOutput != nil {
					if created.Delivery == nil {
						err = fmt.Errorf("create returned no delivery inventory")
					} else {
						err = deliveryOutput.write(*created.Delivery)
					}
				}
			}
		}
	case "pull":
		mode := bale.PullLocked
		if opts.mode == "latest" {
			mode = bale.PullLatest
		}
		var pulled bale.PullResult
		pulled, err = service.PullWithOptions(ctx, ref, opts.args[1], mode, bale.PullOptions{AllowPlugins: opts.allowPlugins})
		result, resultDigest = pulled, pulled.Descriptor.Digest.String()
	case "push":
		var plan bale.DeliveryPlan
		plan, err = service.PlanPush(ctx, opts.args[0], ref, bale.PushOptions{RepositoryMap: maps, Delivery: delivery})
		if err == nil {
			if opts.dryRun {
				result, resultDigest = plan, plan.Descriptor.Digest.String()
			} else {
				var pushed bale.PushResult
				pushed, err = service.Push(ctx, plan)
				result, resultDigest = pushed, pushed.Descriptor.Digest.String()
			}
		}
	}
	if err != nil || progressErr != nil {
		// A transfer can confirm work before failing. Preserve that report only
		// for explicit JSON output; a digest or template could imply success.
		var outputErr error
		if formatter.json && !opts.dryRun && resultDigest != "" {
			outputErr = formatter.writeResult(out, result, resultDigest)
		}
		return fail(errOut, errors.Join(err, progressErr, outputErr))
	}
	var outputErr error
	if command == "create" && opts.dryRun && !opts.formatSet {
		outputErr = writeCreatePreview(out, result.(bale.CreateResult))
	} else if command == "push" && opts.dryRun && !formatter.json && formatter.template == nil {
		outputErr = writePushPreview(out, result.(bale.DeliveryPlan))
	} else {
		outputErr = formatter.writeResult(out, result, resultDigest)
	}
	if err := errors.Join(outputErr, progressErr); err != nil {
		return fail(errOut, err)
	}
	return 0
}

func newCLIClient(opts commandOptions, progress func(bale.Progress)) (*bale.Client, error) {
	options := bale.ClientOptions{PlainHTTPRegistries: opts.plainHTTP, OnProgress: progress}
	options.PluginRunner = configuredPluginRunner(opts)
	// Credential discovery is lazy: local reads stay offline, while operations
	// with local inputs can still authenticate their registry destinations.
	options.Credentials = lazyDockerCredentials(opts.authFile)
	return bale.NewClient(options)
}

func runInspect(ctx context.Context, opts commandOptions, formatter *outputFormatter, out, errOut io.Writer) int {
	client, err := newCLIClient(opts, nil)
	if err != nil {
		return fail(errOut, err)
	}
	results := make([]bale.InspectResult, 0, len(opts.args))
	var failures []error
	for _, arg := range opts.args {
		var result bale.InspectResult
		if opts.layout {
			result, err = client.InspectLayout(ctx, arg)
		} else if err = explicitReference(arg, false); err == nil {
			result, err = client.Inspect(ctx, arg)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", arg, err))
		} else {
			results = append(results, result)
		}
		if ctx.Err() != nil {
			break
		}
	}
	failures = append(failures, formatter.writeInspections(out, results))
	if err := errors.Join(failures...); err != nil {
		return fail(errOut, err)
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
