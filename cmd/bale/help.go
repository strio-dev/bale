package main

func commandHelp(command string) string {
	var usage, description, options, example string
	switch command {
	case "create":
		usage = "bale create [options]"
		description = "Resolve a recipe into the managed local store without downloading image payloads.\nNames are local aliases; creation without --tag retains the bundle by digest.\nUse --layout PATH for an explicit complete OCI directory, or --registry REF to store remotely."
		options = `  -f, --file FILE         Recipe file (required; '-' reads stdin)
  -t, --tag NAME         Local name for the captured bundle (repeatable)
      --layout            Use a positional output directory and download complete content
      --registry          Capture and publish to the given registry reference
      --dry-run           Preview resolved artifact inventory without publication
      --output-delivery FILE  Write captured delivery YAML to a new file`
		description += "\nConfigured OCI discovery plugins run automatically."
		options += "\n" + pluginRuntimeOptions
		example = "bale create -f bale.yaml -t release:v1 --output-delivery bale.delivery.yaml"
		example += "\n  bale create --layout -f bale.yaml ./offline"
		example += "\n  bale create --registry -f bale.yaml registry.example.com/bundles/release:v1"
	case "save":
		usage = "bale save [options] BUNDLE"
		description = "Save a stored name or ID as a verified offline OCI tar archive.\nMissing pinned payloads are downloaded into the shared local store first.\nUse --layout to read an explicit OCI directory instead."
		options = "  -o, --output FILE       New OCI tar archive (default: stdout; '-' for stdout)\n      --layout            Read an explicit OCI directory"
		example = "bale save release:v1 -o release.tar"
	case "load":
		usage = "bale load [options]"
		description = "Import a verified OCI bundle archive into the managed local store.\nUse --tag for local names, or --layout PATH for an explicit directory.\nLoading uses no registry access or plugin execution."
		options = "  -i, --input FILE        OCI tar archive (default: stdin; '-' for stdin)\n  -t, --tag NAME         Local name for the imported bundle (repeatable)\n      --layout            Restore into a positional new OCI directory"
		example = "bale load -i release.tar -t restored:v1"
	case "inspect":
		usage = "bale inspect [options] BUNDLE..."
		description = "Inspect a stored bundle by local name or immutable ID.\nMetadata captures are inspected without source access; complete bundles also verify payloads.\nUse --registry for remote references, or --layout for explicit OCI directories."
		options = `      --registry          Inspect registry references instead of stored bundles
      --layout            Inspect explicit local OCI layout directories
  -f, --format FORMAT     Output JSON or a Go template (default: json)`
		example = "bale inspect release:v1\n  bale inspect --registry registry.example.com/bundles/release:v1"
	case "diff":
		usage = "bale diff [options] BEFORE AFTER"
		description = "Compare captured artifacts, selector resolutions and dependency declarations.\nLocal names and IDs resolve in the managed store. Use --registry for remote bundles.\nUse --layout for two explicit OCI directories.\nComparison reads metadata without refreshing sources, running plugins or reading opaque payloads.\nSuccessful comparisons return status 0 whether or not the bundles differ."
		options = `      --registry          Compare registry references instead of stored bundles
      --layout            Compare explicit local OCI layout directories
  -f, --format FORMAT     Output JSON or a Go template (default: readable report)`
		example = "bale diff release:v1 release:v2\n  bale diff --registry registry.example.com/bundles/release:v1 registry.example.com/bundles/release:v2 --format json"
	case "pull":
		usage = "bale pull [options] BUNDLEREF"
		description = "Download a complete bundle into the managed local store.\nUse --tag for local names, or --layout PATH for an explicit directory."
		options = "  -t, --tag NAME         Local name for the downloaded bundle (repeatable)\n      --layout            Download into a positional new OCI directory\n      --mode MODE         Pull mode: locked|latest (default: locked)\n      --allow-plugins     Execute captured recipe plugins during --mode latest\n" + pluginRuntimeOptions
		example = "bale pull registry.example.com/bundles/release:v1 -t release:v1"
	case "push":
		usage = "bale push [options] BUNDLE DESTREF"
		description = "Push a stored bundle name or ID to a registry and publish its completion tag.\nMetadata captures stream missing payloads from their pinned sources; complete layouts stay source-independent.\nDry run validates local metadata and previews delivery without registry access.\nSource payload availability, destination credentials and existing remote tags are not checked during dry run."
		options = "      --layout            Read an explicit OCI directory instead of a stored bundle\n      --map SOURCE=PATH   Map a source repository to a relative destination path (repeatable)\n      --delivery FILE     Apply artifact paths from captured delivery YAML\n      --dry-run           Preview delivery without publication or registry access"
		example = "bale push release:v1 mirror.example.com/releases/v1:bundle\n  bale push release:v1 mirror.example.com/releases/v1:bundle --delivery bale.delivery.yaml --dry-run"
	case "ls":
		usage = "bale ls [options]"
		description = "List retained local bundles, including unnamed IDs and content availability."
		options = "  -q, --quiet             Print bundle IDs only\n      --no-trunc          Show full bundle digests"
		example = "bale ls\n  bale ls --format json"
	case "tag":
		usage = "bale tag [options] BUNDLE NAME..."
		description = "Add local names to a stored bundle without copying content."
		example = "bale tag release:v1 release:approved"
	case "rm":
		usage = "bale rm [options] NAME|ID..."
		description = "Remove local names or retained bundle IDs. Shared content is reclaimed by prune."
		example = "bale rm release:old"
	case "prune":
		usage = "bale prune [options]"
		description = "Reclaim unreferenced local content while preserving all retained bundles."
		options = "      --dry-run           Report reclaimable content without deleting it"
		example = "bale prune --dry-run"
	case "info":
		usage = "bale info [options]"
		description = "Show the managed local store location."
		example = "bale info"
	case "version":
		return "Display version information\n\nUsage:\n  bale version\n\nOptions:\n  -h, --help    Help for version"
	}
	if command != "inspect" && command != "diff" {
		defaultOutput := "bundle digest"
		switch command {
		case "create":
			defaultOutput += "; dry-run: inventory"
		case "push":
			defaultOutput += "; dry-run: delivery preview"
		case "save":
			defaultOutput = "bundle digest with --output FILE; unavailable for archive stdout"
		case "ls":
			defaultOutput = "readable list"
		case "info":
			defaultOutput = "store path"
		case "prune":
			defaultOutput = "reclaimed content"
		case "tag", "rm":
			defaultOutput = "no output"
		}
		if command != "tag" && command != "rm" {
			options += "\n      --format FORMAT     Output JSON or a Go template (default: " + defaultOutput + ")"
		}
		if command == "create" || command == "pull" || command == "push" || command == "save" || command == "load" {
			options += "\n  -q, --quiet             Suppress progress output"
		}
	}
	if registryCommand(command) {
		options += "\n      --plain-http HOST   Permit HTTP for this exact registry host (repeatable)"
		options += "\n      --authfile FILE     Registry authentication file"
	}
	options += "\n      --store PATH        Override the managed local store\n  -h, --help              Help for " + command
	help := description + "\n\nUsage:\n  " + usage + "\n\nOptions:\n" + options + "\n\nExamples:\n  " + example
	if command == "create" || command == "pull" || command == "push" {
		help += "\n\nRegistry references require an explicit host and tag or digest."
	}
	return help
}
