# Inspection

Read the captured recipe and inventory from a registry bundle or local layout:

```sh
bale inspect registry.example.com/bundles/release:v1
bale inspect --layout ./release
```

The JSON report includes roots, origins, referrers, resolved dependencies, plugin provenance and selected declaration JSON. Inspection validates the complete graph, including nested Bale metadata, without refreshing tags or running plugins. Registry inspection reads payloads and can take time for large bundles; local inspection stays offline. Digest validation does not verify publisher signatures.

## Compare snapshots

Pull locked and refreshed snapshots, then compare them:

```sh
bale pull registry.example.com/bundles/release:v1 ./locked
bale pull registry.example.com/bundles/release:v1 ./refreshed --mode latest
bale diff --layout ./locked ./refreshed
```

Add `--allow-plugins` to latest pull if the recipe uses discovery plugins. Diff itself never runs plugins.

The report shows bundle and recipe changes, added or removed artifacts, moved selectors and changed dependency observations. Artifact identity is source repository plus digest; opaque configs and payload blobs are excluded. A tag moving to content already captured elsewhere still appears as a selector change.

You can compare registry references directly or request structured output:

```sh
bale diff registry.example.com/bundles/release:v1 registry.example.com/bundles/release:v2
bale diff --layout ./locked ./refreshed --format json
bale diff --layout ./locked ./refreshed -f '{{.Changed}}'
```

Each registry reference resolves once. Comparing a mutable tag with itself uses the same pinned snapshot; retain separate layouts or references to compare changes over time. Repeated selectors are compared as sets rather than paired by input position. Detailed selector and declaration changes describe the root bundles; nested inventory membership is included, and nested bundles can be compared separately.

Diff reads manifests and interpreted metadata, not opaque configs or payload layers. Use ordinary inspection to check the complete captured content. A successful comparison exits 0 even when bundles differ; errors exit 125 without a comparison report.

## Export delivery YAML

Export default publication paths before pulling or from a local layout:

```sh
bale inspect registry.example.com/bundles/release:v1 \
  --output-delivery bale.delivery.yaml
bale inspect --layout ./release --output-delivery local.delivery.yaml
```

Export accepts exactly one bundle, replaces stdout inspection output and cannot be combined with `--format`. It requires a new output file and regenerates defaults; save any authored path edits separately. See [delivery paths](transfers.md#export-delivery-paths).

Registry export reads captured manifests and interpreted metadata, including nested bundles and declarations, without downloading or checking opaque payloads. It does not prove payload availability or integrity. Local export validates the complete graph. Neither form refreshes original selectors or discovers new referrers.

## JSON and templates

Inspection returns a JSON array, even for one bundle. Use a Go template to select fields:

```sh
bale inspect --layout ./release -f '{{.Descriptor.Digest}}'
bale inspect --layout ./release -f '{{json .Metadata.Dependencies}}'
bale inspect --layout ./release -f '{{json .Metadata.Plugins}}'
```

Multiple inputs are supported. If one fails, successful inspections remain in stdout, errors go to stderr and the command exits 125. See [CLI output and errors](reference/cli.md) for formats and automation.

## Nested bundles and SDK access

Nested bundles retain their own inventories and can be inspected by pinned digest; their metadata is validated when inspecting the outer bundle too. The [Go SDK](sdk.md#inspect-a-bundle) supports registry references, layouts and caller-owned stores, with matching [comparison methods](sdk.md#compare-bundles).
