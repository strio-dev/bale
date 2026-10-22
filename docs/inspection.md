# Inspection

Inspect a registry bundle or a completed local OCI layout:

```sh
bale inspect registry.example.com/bundles/release:v1
bale inspect --layout ./release
```

Inspection reports the captured recipe, roots, origins, referrer associations, resolved dependency digests, plugin provenance and selected declaration JSON. It validates the complete captured graph and every nested native bundle's metadata without refreshing source tags, executing plugins or discovering new attachments.

Registry inspection reads and hashes captured payloads, so large bundles can take time. Local layout inspection performs no network access or writes and does not load registry credentials. Content integrity validation does not verify publisher signatures.

## Compare snapshots

Review a refresh before delivering it:

```sh
bale pull registry.example.com/bundles/release:v1 ./locked
bale pull --mode latest registry.example.com/bundles/release:v1 ./refreshed
bale diff --layout ./locked ./refreshed
```

If the captured recipe uses discovery plugins, add `--allow-plugins` to the latest pull. Comparison itself never executes plugins.

The readable report shows both bundle digests, whether the recipe changed, added and removed artifacts, changed input/dependency/plugin selector resolutions, and changed dependency declaration observations. For example:

```text
Bundle changed: true
Recipe changed: false
Artifacts: 1 added, 1 removed
Selector resolutions: 1 changed
Dependency observations: 0 changed
```

Full artifact identities and before/after digest sets follow these counts. Artifacts are identified by source repository and digest; configs and payload blobs are excluded. A selector moving to bytes already present elsewhere in the bundle still appears as a resolution change. Repeated selectors and multiple discovery profiles use sets, without pairing inputs by position. Recipe ordering and plugin input changes still appear through `recipeChanged`, even if the included artifacts stay the same.

Compare registry bundles directly, or request one JSON object or a Go template:

```sh
bale diff registry.example.com/bundles/release:v1 registry.example.com/bundles/release:v2
bale diff --layout ./locked ./refreshed --format json
bale diff --layout ./locked ./refreshed -f '{{.Changed}}'
bale diff --layout ./locked ./refreshed -f '{{json .Resolutions}}'
```

Each registry reference is resolved once. Identical reference strings share one pinned inspection, so comparing a mutable tag with itself compares the same snapshot. Use separate captured references or layouts to review changes over time.

Diff reads and validates supported OCI envelopes, interpreted Bale metadata and selected declaration JSON, including nested native bundle metadata. It does not refresh original selectors, discover attachments, publish content, or download or check the existence of opaque configs and payload layers. A successful comparison does not prove payload availability or integrity. Use ordinary inspection for complete captured-content validation. Local comparison makes no network requests, reads no registry credentials and leaves both layouts unchanged.

Detailed selector and declaration changes describe the compared root bundles. Nested bundles retain their own observations and can be compared separately by their pinned references. Nested captured artifact membership remains part of the outer inventory.

Successful comparisons return status 0, including when bundles differ. Errors return status 125 with no comparison report; neither input is partially reported. JSON exposes `before`, `after`, `changed`, `recipeChanged`, `artifacts`, `resolutions` and `declarations`. Empty change collections are arrays.

## Export delivery YAML

Export editable delivery paths before pulling, or from an existing layout:

```sh
bale inspect registry.example.com/bundles/release:v1 \
  --output-delivery bale.delivery.yaml
bale inspect --layout ./release --output-delivery local.delivery.yaml
```

`--output-delivery` accepts exactly one bundle, writes a new file, and replaces the normal stdout inspection report. It cannot be combined with `--format`.

Registry export resolves the bundle reference once, then reads and hashes supported OCI manifests and indexes plus interpreted Bale metadata and declaration JSON. It validates captured source coordinates, inventory membership, subjects, dependencies and nested bundle metadata without resolving original sources or discovering referrers. It does not download or check the existence of opaque application configs or payload layers, so it does not prove their availability or integrity. Ordinary inspection, pull and push planning retain full graph validation.

Local export validates the complete captured graph without network access or modifying the layout; only the requested output file is written.

The file is regenerated from captured metadata with default paths, so previously edited paths must be saved separately. See [delivery paths](transfers.md#export-delivery-paths) for the schema and push behavior.

## JSON and templates

Inspection accepts multiple inputs and returns an indented JSON array by default, including for a single bundle. `--format json` selects that output explicitly. `--format/-f` can instead apply a Go template to each successful result:

```sh
bale inspect --layout ./release -f '{{.Descriptor.Digest}}'
bale inspect --layout ./release -f '{{json .Metadata.Dependencies}}'
bale inspect --layout ./release -f '{{json .Metadata.Plugins}}'
bale inspect --layout ./release ./other-release --format json
```

The `json` helper encodes the selected value. `{{json .}}` emits one JSON object per successful inspection. Batch inspection retains successful results when another input fails, prints individual errors to stderr and returns status 125. When every input fails, JSON output is an empty array.

## Nested bundles and SDK access

The report describes the inspected bundle. Nested bundles keep their own inventories and can be inspected separately by their pinned digest. Their metadata is validated even when reporting the outer bundle.

The [Go SDK](sdk.md#inspect-a-bundle) provides `Inspect`, `InspectLayout` and `InspectRoot` for registry references, completed layouts and caller-owned stores. All return verified metadata and selected declarations without changing the captured resolutions. Use [`Diff`, `DiffLayouts` or `DiffRoots`](sdk.md#compare-bundles) to compare captured snapshots.
