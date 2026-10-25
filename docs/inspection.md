# Inspect and compare bundles

Read a captured recipe and inventory by its local name or bundle ID. Choose `--registry` to inspect a remote bundle:

```sh
bale inspect --registry registry.example.com/bundles/release:v1
bale inspect release:v1
```

The JSON report includes captured artifacts, origins, referrers, dependencies and selected declarations. A local metadata capture reports `metadataOnly: true`: inspection verifies its manifests and interpreted metadata without contacting sources or checking payload availability. Complete layouts and registry bundles receive full payload verification, which can take time for large bundles. Digest checks do not verify publisher signatures.

## Compare snapshots

Pull the captured and refreshed versions, then compare them:

```sh
bale pull registry.example.com/bundles/release:v1 -t release:locked
bale pull registry.example.com/bundles/release:v1 -t release:refreshed --mode latest
bale diff release:locked release:refreshed
```

Add `--allow-plugins` to latest pull when the recipe uses discovery plugins. Diff reports recipe changes, added or removed artifacts, moved selectors and changed dependencies. It reads metadata without running plugins or checking payloads.

Registry references and structured output work too:

```sh
bale diff --registry registry.example.com/bundles/release:v1 registry.example.com/bundles/release:v2
bale diff release:locked release:refreshed --format json
bale diff release:locked release:refreshed -f '{{.Changed}}'
```

A successful comparison exits 0 even when bundles differ. See [comparison rules](reference/cli.md#inspection-and-comparison) for mutable tags, nested inventories and artifact identity.

## Export delivery YAML

Export publication paths from a stored bundle or a remote bundle:

```sh
bale delivery export --registry registry.example.com/bundles/release:v1 \
  -o bale.delivery.yaml
bale delivery export release:v1 -o local.delivery.yaml
bale delivery export --layout ./offline
```

Export takes one bundle and writes YAML to stdout by default, including with `-o -`. Use `-o FILE` for a new file and edit its paths before [delivery](transfers.md#export-delivery-paths).

Registry export and export from a local metadata capture verify metadata without downloading payloads or checking their availability. Complete stored bundles and explicit file layouts receive full graph validation. Re-exporting produces defaults rather than recovering earlier edits.

## JSON and templates

Inspection returns a JSON array. Select fields with a Go template:

```sh
bale inspect release:v1 -f '{{.Descriptor.Digest}}'
bale inspect release:v1 -f '{{json .Metadata.Dependencies}}'
bale inspect release:v1 -f '{{json .Metadata.Plugins}}'
```

For multiple inputs, successful results remain in stdout if another input fails. Errors go to stderr and return status 125. See [CLI output](reference/cli.md).

## Nested bundles and SDK access

Inspect nested bundles separately by pinned digest. The [Go SDK](sdk.md#inspect-a-bundle) also supports the managed store, explicit layouts and caller-owned content stores, with matching [comparison methods](sdk.md#compare-bundles).
