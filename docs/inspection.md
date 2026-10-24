# Inspect and compare bundles

Read a captured recipe and inventory from a registry or local layout:

```sh
bale inspect registry.example.com/bundles/release:v1
bale inspect --layout ./release
```

The JSON report includes captured artifacts, origins, referrers, dependencies and selected declarations. A local metadata capture reports `metadataOnly: true`: inspection verifies its manifests and interpreted metadata without contacting sources or checking payload availability. Complete layouts and registry bundles receive full payload verification, which can take time for large bundles. Digest checks do not verify publisher signatures.

## Compare snapshots

Pull the captured and refreshed versions, then compare them:

```sh
bale pull registry.example.com/bundles/release:v1 ./locked
bale pull registry.example.com/bundles/release:v1 ./refreshed --mode latest
bale diff --layout ./locked ./refreshed
```

Add `--allow-plugins` to latest pull when the recipe uses discovery plugins. Diff reports recipe changes, added or removed artifacts, moved selectors and changed dependencies. It reads metadata without running plugins or checking payloads.

Registry references and structured output work too:

```sh
bale diff registry.example.com/bundles/release:v1 registry.example.com/bundles/release:v2
bale diff --layout ./locked ./refreshed --format json
bale diff --layout ./locked ./refreshed -f '{{.Changed}}'
```

A successful comparison exits 0 even when bundles differ. See [comparison rules](reference/cli.md#inspection-and-comparison) for mutable tags, nested inventories and artifact identity.

## Export delivery YAML

Export publication paths from a stored bundle or layout:

```sh
bale inspect registry.example.com/bundles/release:v1 \
  --output-delivery bale.delivery.yaml
bale inspect --layout ./release --output-delivery local.delivery.yaml
```

Use a new file and edit its paths before [delivery](transfers.md#export-delivery-paths). Export takes one bundle and replaces normal inspection output; it cannot be combined with `--format`.

Registry export and export from a local metadata capture verify metadata without downloading payloads or checking their availability. Complete local layouts receive full graph validation. Re-exporting produces defaults rather than recovering earlier edits.

## JSON and templates

Inspection returns a JSON array. Select fields with a Go template:

```sh
bale inspect --layout ./release -f '{{.Descriptor.Digest}}'
bale inspect --layout ./release -f '{{json .Metadata.Dependencies}}'
bale inspect --layout ./release -f '{{json .Metadata.Plugins}}'
```

For multiple inputs, successful results remain in stdout if another input fails. Errors go to stderr and return status 125. See [CLI output](reference/cli.md).

## Nested bundles and SDK access

Inspect nested bundles separately by pinned digest. The [Go SDK](sdk.md#inspect-a-bundle) also supports layouts and caller-owned stores, with matching [comparison methods](sdk.md#compare-bundles).
