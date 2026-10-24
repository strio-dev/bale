package main

func commandHelp(command string) string {
	var usage, description, options, example string
	switch command {
	case "create":
		usage = "bale create [options] PATH|STORAGEREF"
		description = "Resolve a recipe into a new local metadata capture, without downloading image payloads.\nThe capture pins source digests; later push or save reads missing content from those sources.\nUse --layout to download a complete offline layout, or --registry to store a bundle in a registry."
		options = `  -f, --file FILE         Recipe file (required)
      --layout            Download a complete new local OCI layout
      --registry          Capture and publish to the given registry reference
      --dry-run           Preview resolved artifact inventory without publication
      --output-delivery FILE  Write captured delivery YAML to a new file`
		description += "\nConfigured OCI discovery plugins run automatically."
		options += "\n" + pluginRuntimeOptions
		example = "bale create -f bale.yaml ./release --output-delivery bale.delivery.yaml"
		example += "\n  bale create --layout -f bale.yaml ./offline"
		example += "\n  bale create --registry -f bale.yaml registry.example.com/bundles/release:v1"
	case "save":
		usage = "bale save [options] LAYOUT"
		description = "Save a local bundle as a new, verified offline OCI tar archive.\nMetadata captures download missing payloads from their pinned sources; complete layouts stay offline.\nThe archive preserves the captured bundle digest and contains each required blob once."
		options = "  -o, --output FILE       New OCI tar archive (required)"
		example = "bale save ./release -o release.tar"
	case "load":
		usage = "bale load [options] LAYOUT"
		description = "Restore an OCI bundle archive into a new local layout.\nThe archive is validated without network access, credentials or plugin execution."
		options = "  -i, --input FILE        OCI tar archive (required)"
		example = "bale load -i release.tar ./restored"
	case "inspect":
		usage = "bale inspect [options] BUNDLE..."
		description = "Inspect verified bundle inventory, selected declarations and captured digests.\nRegistry references are inspected by default. Use --layout for local OCI directories.\nLocal metadata captures report metadataOnly and validate captured metadata without source access.\nComplete bundles also verify payload bytes. Inspection never refreshes source tags."
		options = `      --layout            Interpret all operands as local OCI layout directories
      --output-delivery FILE  Export delivery YAML for one bundle instead of a report
  -f, --format FORMAT     Output JSON or a Go template (default: json)`
		example = "bale inspect registry.example.com/bundles/release:v1\n  bale inspect --layout ./release -f '{{.Descriptor.Digest}}'"
	case "diff":
		usage = "bale diff [options] BEFORE AFTER"
		description = "Compare captured artifacts, selector resolutions and dependency declarations.\nRegistry references are compared by default. Use --layout for two OCI directories.\nComparison reads metadata without refreshing sources, running plugins or reading opaque payloads.\nSuccessful comparisons return status 0 whether or not the bundles differ."
		options = `      --layout            Interpret both operands as local OCI layout directories
  -f, --format FORMAT     Output JSON or a Go template (default: readable report)`
		example = "bale diff --layout ./locked ./refreshed\n  bale diff registry.example.com/bundles/release:v1 registry.example.com/bundles/release:v2 --format json"
	case "pull":
		usage = "bale pull [options] BUNDLEREF LAYOUT"
		description = "Pull a bundle into a new local OCI layout."
		options = "      --mode MODE         Pull mode: locked|latest (default: locked)\n      --allow-plugins     Execute captured recipe plugins during --mode latest\n" + pluginRuntimeOptions
		example = "bale pull registry.example.com/bundles/release:v1 ./release"
	case "push":
		usage = "bale push [options] LAYOUT DESTREF"
		description = "Push a local bundle to a registry and publish its completion tag.\nMetadata captures stream missing payloads from their pinned sources; complete layouts stay source-independent.\nDry run validates local metadata and previews delivery without registry access.\nSource payload availability, destination credentials and existing remote tags are not checked during dry run."
		options = "      --map SOURCE=PATH   Map a source repository to a relative destination path (repeatable)\n      --delivery FILE     Apply artifact paths from captured delivery YAML\n      --dry-run           Preview delivery without publication or registry access"
		example = "bale push ./release mirror.example.com/releases/v1:bundle\n  bale push ./release mirror.example.com/releases/v1:bundle --delivery bale.delivery.yaml --dry-run"
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
		}
		options += "\n      --format FORMAT     Output JSON or a Go template (default: " + defaultOutput + ")\n  -q, --quiet             Suppress progress output"
	}
	if command != "load" {
		options += "\n      --plain-http HOST   Permit HTTP for this exact registry host (repeatable)"
	}
	options += "\n  -h, --help              Help for " + command
	help := description + "\n\nUsage:\n  " + usage + "\n\nOptions:\n" + options + "\n\nExamples:\n  " + example
	if command != "save" && command != "load" {
		help += "\n\nRegistry references require an explicit host and tag or digest."
	}
	return help
}
