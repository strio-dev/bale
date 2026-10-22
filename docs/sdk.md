# Go SDK

Go applications import `github.com/strio-dev/bale`. The SDK transfers OCI graphs from registries or caller-owned ORAS stores, captures bundles, and inspects verified bundle metadata. See [getting started](getting-started.md) for the bundle workflow.

## Configure a client

`bale.NewClient(bale.ClientOptions{})` uses anonymous authentication. Supply `ClientOptions.Credentials` for authenticated requests; the SDK does not read Docker credential files. The CLI explicitly loads them.

Callers can inject an `HTTPClient` for TLS and transport policy, allow exact HTTP registry hosts through `PlainHTTPRegistries`, and receive synchronous `OnProgress` callbacks.

Capture progress includes `resolving`, `resolving-plugin`, `fetching-plugin`, `running-plugin`, `capturing` and `captured` operations. Recipe events carry an optional zero-based `Input`, the selected artifact `Reference` and configured `Plugin`; `Descriptor` identifies the pinned subject when available. Fetch and run events precede those operations, and `running-plugin` covers both compilation and execution through the caller's runner. `captured` means the snapshot was built; publication remains reported separately through `copied` and `tagged`. Dry runs report capture progress without publication events.

Callbacks remain serialized and synchronous. Capture notifications are advisory and may be skipped when another operation's callback is active, so a blocked observer cannot stall unrelated capture or cancellation while waiting to publish. Return promptly from callbacks and use the operation result to determine completion.

`ParseRecipe` parses strict YAML and resolves authoring defaults. `NormalizeRecipe` applies the same defaults to a deep copy of a Go recipe. Bundle operations are available through `Create`, `Pull`, `PlanPush`, and `Push`.

## Capture and archive locally

```go
captured, err := client.CreateLayout(ctx, recipe, "./release", bale.CreateOptions{
    ExportDelivery: true,
})
if err != nil {
    return err
}
saved, err := client.Save(ctx, "./release", "./release.tar")
if err != nil {
    return err
}
restored, err := client.Load(ctx, "./release.tar", "./restored")
if err != nil {
    return err
}
// captured.Descriptor, saved.Descriptor and restored.Descriptor are identical.
```

`CreateLayout` captures the same recipe, dependencies and configured plugins as `Create`, using read access to source registries. It installs a new complete OCI layout without registry publication. `CreateOptions.DryRun` captures and validates without installing it; `ExportDelivery` returns paths from that exact capture. `CreateResult.LayoutPath` is set only after successful local installation, while `Published` reports registry publication only.

`Save` and `Load` return `ArchiveResult` with `Descriptor`, `LayoutPath` and `ArchivePath`. They validate the complete captured graph, retain original digests and store shared blobs once in an uncompressed OCI-layout tar archive. Both are offline: no credentials, plugin execution or registry access. Destinations must be new paths. A restored layout works with `InspectLayout`, `DiffLayouts`, `UpdateDeliveryLayouts`, `PlanPush` and `Push`; those methods take a layout directory rather than an archive filename. See [offline packaging](offline.md) for the CLI workflow.

## Transfer an artifact graph

Direct graph transfers do not require a bundle. This snippet assumes an existing `context.Context` named `ctx` in a function returning an error, and imports Bale as `bale`:

```go
client, err := bale.NewClient(bale.ClientOptions{})
if err != nil {
    return err
}
root, err := client.Resolve(ctx, "registry.example.com/components/tool:v1")
if err != nil {
    return err
}
plan, err := client.Plan(ctx, bale.TransferRequest{
    Roots: []bale.Root{root},
    Destination: "mirror.example.com/release",
    RepositoryMap: map[string]string{
        root.Repository: "components/tool",
    },
})
if err != nil {
    return err
}
result, err := client.Execute(ctx, plan)
// Inspect result even when err is non-nil: it retains confirmed publications.
```

`Plan` reads and validates the graph without publishing. `plan.View()` returns a defensive inspection copy. Execution uses pinned descriptors. Plans are in-memory objects; their JSON inspection view is not an executable serialized plan.

`result.Relocations.Lookup(bale.ArtifactID{Repository: root.Repository, Digest: root.Descriptor.Digest})` returns the confirmed destination coordinate. Tags are reported separately, and publication across tags is not atomic. Inspect results on failure to distinguish confirmed publications from planned work.

### Caller-owned content and discovery

For local content, construct a `bale.Root` with its repository provenance, descriptor, caller-owned `content.ReadOnlyStorage`, and optional aliases. Keep the store available through execution.

Set `Referrers: bale.ReferrerPolicy{Include: "none"}` when no discovery provider is available. The default policy selects all and needs a `ReferrerSource`; `Resolve` supplies its registry provider. Required referrers remain additive and are validated against their subjects.

Caller-supplied discovery providers control their own listing strategy, including ORAS native/fallback choices. Bale verifies each selected attachment's bytes and subject. Bale-created registry providers and destination confirmation share strict inventory and referrer validation.

Application integrations own application-specific dependency discovery and reference rewriting. They can declare dependencies hidden in application payloads and consume the resulting relocation map.

Bundle push also publishes [destination records](destinations.md) for later consumers that do not retain the operation result.

## Author dependency declarations

Prepare a declaration and publish it using the attachment operation. This snippet continues with the client and context above:

```go
subject, err := client.Resolve(ctx, "registry.example.com/charts/web:1.4")
if err != nil {
    return err
}
attachment, err := bale.PrepareDependencyDeclaration(ctx, subject,
    bale.DependencyDeclaration{
        Dependencies: []bale.DependencyReference{
            {Ref: "registry.example.com/images/web:stable"},
        },
    })
if err != nil {
    return err
}
attached, err := client.Attach(ctx, attachment)
// Inspect attached.Published and attached.Discoverable even on error.
```

`PrepareDependencyDeclaration` builds a deterministic local metadata store without reading the subject's source store or resolving dependency targets. `ParseDependencyDeclaration` parses strict authoring YAML with an implicit schema version, rejects an explicit `schemaVersion` field, requires a `dependencies` array, and validates references and policies without registry access. `EncodeDependencyDeclaration` and `DecodeDependencyDeclaration` provide strict JSON authoring and validation; registry JSON carries `schemaVersion: 1`. The CLI exposes this workflow through [`bale dependencies attach`, `list`, and `inspect`](dependencies.md#author-and-attach-a-declaration).

`Create` and latest `Pull` interpret these declarations. Direct `Plan`/`Execute` transfers only the supplied immutable OCI graph and selected referrers, without resolving declaration references. Use a bundle to capture an offline dependency set. See [dependency declarations](dependencies.md) for the schema and inclusion rules.

### Run a dependency plugin

Import `github.com/strio-dev/bale/plugin` as `plugin`. Opt into executing configured recipe plugins by injecting a runner when constructing the client:

```go
client, err := bale.NewClient(bale.ClientOptions{
    PluginRunner: plugin.Runner(plugin.Options{}),
})
if err != nil {
    return err
}
```

`Create` runs the plugins selected in the recipe. The client resolves their OCI graphs, verifies module bytes, assembles literal input and blob bindings, and invokes the supplied runner. The resulting bundle retains plugin provenance and generated declarations without publishing source attachments. A client without a runner rejects recipes that require execution.

Locked pull and push do not execute plugins. For latest pull of a plugin recipe, additionally authorize execution through `client.PullWithOptions(ctx, reference, layout, bale.PullLatest, bale.PullOptions{AllowPlugins: true})`.

For a manual registry run, supply a plugin-defined JSON request (`encoding/json` is imported as `json`):

```go
result, err := plugin.RunOCI(ctx, client, plugin.OCIRequest{
    Ref: "registry.example.com/plugins/references:1",
    Input: json.RawMessage(`{"refs":["registry.example.com/images/web:stable"]}`),
}, plugin.Options{})
if err != nil {
    return err
}
// result.Descriptor pins the plugin's resolved OCI root.
attachment, err := bale.PrepareDependencyDeclaration(ctx, subject, result.Declaration)
if err != nil {
    return err
}
attached, err := client.Attach(ctx, attachment)
// Inspect attached.Published and attached.Discoverable even on error.
```

Registry credentials and transport policy come from the supplied client. Manual input must contain any archive bytes the plugin needs; recipe blob bindings apply during capture. `plugin.Run(ctx, plugin.Request{ModulePath: "references.wasm", Input: input}, options)` remains available for explicit local development.

`plugin.Pack(ctx, store, modulePath)` writes a portable Wasm OCI image to caller-owned `content.Storage` and returns its manifest descriptor. Publish that graph using the existing transfer API; the CLI exposes packaging and publication through `bale plugins push`.

`Options` exposes `ExecutionTimeout`, `MemoryLimitBytes`, and `CacheDirectory`. Zero timeout and memory values use 30 seconds and 256 MiB; an empty cache directory disables persistent caching. The SDK does not choose a user cache directory. Cached native code is private to the runtime and must remain local and trusted.

The package exposes no Wago types. Plugins use the portable WASI Preview 1 command contract and strict declaration JSON, so an engine replacement can preserve caller and guest interfaces. Runs serialize within the process; waiting is cancellable, but synchronous compilation can delay cancellation. Execution time and linear-memory limits do not cap compilation time or total process memory. See [dependency plugins](plugins.md) for the input/output contract, fixed size limits, and a complete Go guest example.

## Publish and discover attachments

Publish a prepared OCI attachment with `client.Attach(ctx, bale.AttachRequest{Subject: subjectReference, Descriptor: attachmentDescriptor, Store: store})`. The subject must already exist in its registry. The caller prepares an OCI manifest or index whose `subject` matches that object and supplies its complete strong dependency closure in a caller-owned store.

Bale preserves the attachment bytes, publishes into the subject's repository, verifies the uploaded closure, and confirms referrer discovery. It repairs a missing referrer inventory entry on retry, including when the attachment manifest already exists.

Inspect `AttachResult.Published` and `Discoverable` separately on errors; they report confirmed work. Upload statistics count successful SDK graph operations. They can undercount bytes when a manifest upload succeeds but its inventory update fails. `Published` uses independent verification of remote content.

`client.ListReferrers(ctx, subjectReference, bale.ListReferrersOptions{ArtifactType: "application/vnd.example.dependencies.v1"})` returns sorted, verified OCI referrer descriptors. Omit `ArtifactType` to list all. It verifies actual envelope bytes and subjects before applying the effective artifact type filter; an OCI manifest without an explicit artifact type uses its config media type. A failure can return already validated entries alongside the error.

`client.InspectDependencies(ctx, subjectReference)` returns all verified attached Bale dependency declarations as `[]bale.InspectedDeclaration`, sorted by declaration digest. Each record includes `Repository`, the pinned `Subject`, the declaration `Descriptor`, and the parsed `Declaration`, including its context and dependency entries. The method reads declaration metadata without resolving dependency references or applying a selection. Empty results are a non-nil slice; failure returns no data. The CLI exposes this through `bale dependencies inspect SUBJECT`, with JSON output by default and per-record Go templates via `-f`/`--format`.

### Inspect delivered destinations

`client.InspectDestinations(ctx, subjectReference)` returns `[]bale.InspectedDestination`, sorted by attachment digest. Each entry includes `Repository`, pinned `Subject`, attachment `Descriptor`, parsed `Record` and `Local`. It validates bounded registry metadata without reading application payloads or resolving recorded locations. Empty results are a non-nil slice; errors return no records.

Use `client.InspectDestinationsWithOptions(ctx, reference, bale.DestinationInspectionOptions{MaxMetadataBytes: 4 << 20, MaxReferrers: 256})` to bound aggregate inventory and attachment reads before fetching more content. The byte budget includes validation rereads; the referrer limit counts advertised entries. Zero limits preserve the original inspection behavior. Errors return no partial records. These limits do not cap process memory.

`Local` identifies a record whose destination matches the queried repository and digest. Callers select the intended source, bundle, capture and declaration explicitly, rejecting ambiguity; local does not imply newest, completed or currently available. Historical records can coexist with local ones.

`PlanPush` exposes sealed `DeliveryPlan.Destinations` as `[]DestinationAttachment`. `PushResult.Destinations` reports each attempted attachment through `AttachResult`, including `Published` and `Discoverable` even on error. Canonical artifact `Relocations` remain separate from generated metadata. `EncodeDestinationRecord` and `DecodeDestinationRecord` validate the strict JSON representation without registry access; `DestinationArtifactType`, `DestinationVersion` and `DestinationMaxBytes` describe the wire contract. See [destination records](destinations.md) for fields and publication limits.

### Publication coordination

Registry publication through `Attach`, `Create`, `Execute`, and `Push` serializes per client and allows cancellation while waiting. Read-only registry operations and dry runs do not acquire this permit.

Coordinate overlapping publication by other clients or processes in the calling application. Fallback inventory updates do not provide distributed atomicity. See [registry behavior and retention](registry.md) for publication and garbage-collection constraints.

## Inspect a bundle

Read a verified bundle without creating a layout or planning a delivery. This snippet uses the same client and context:

```go
inspection, err := client.Inspect(ctx, "registry.example.com/bundles/release:v1")
if err != nil {
    return err
}
// inspection.Metadata contains the captured recipe, roots, origins,
// referrer associations, dependency resolutions and plugin provenance.
// inspection.Declarations contains the selected declaration JSON and subjects.
```

The SDK offers three entry points:

| Method | Source |
| --- | --- |
| `Inspect(ctx, reference)` | A registry bundle reference, resolved once. |
| `InspectLayout(ctx, path)` | A completed local OCI bundle layout, without network access or writes. |
| `InspectRoot(ctx, root)` | A pinned bundle descriptor in a caller-owned read-only store. |

Each returns `InspectResult` with `Descriptor`, `Metadata`, and `Declarations`. `Metadata.Dependencies` records original selectors and resolved digests. `Metadata.Plugins` records each configured input, subject, original plugin reference, pinned plugin root and generated declaration descriptor. `Declarations` pairs selected declaration JSON with its subject and source repository.

All three APIs verify the complete captured graph and nested native bundle metadata before returning a report. Registry inspection reads and hashes captured payloads, so large bundles can take time. Inspection does not refresh source selectors or discover additional referrers.

Reports describe the inspected bundle. Nested bundles retain their own inventories; pass a nested bundle's pinned descriptor and the same store to `InspectRoot` to inspect it separately. `InspectRoot` does not close the caller's store or change its ownership.

Failure returns an empty report and a typed error. Content integrity validation does not verify publisher signatures. See [inspection](inspection.md) for CLI output and templates.


## Compare bundles

```go
changes, err := client.DiffLayouts(ctx, "./locked", "./refreshed")
if err != nil {
    return err
}
// changes.Changed compares bundle digests; RecipeChanged compares recipes.
// Artifacts reports added/removed source repository + digest identities.
// Resolutions compares descriptor sets for original input, dependency and plugin refs.
// Declarations compares dependency policy, outcome, selected declaration and context.
```

`Diff(ctx, beforeRef, afterRef)` resolves each registry operand once; identical reference strings share one pinned inspection. `DiffLayouts(ctx, beforePath, afterPath)` reads two single-root OCI layouts without network access, credentials or writes. `DiffRoots(ctx, beforeRoot, afterRoot)` uses caller-owned read-only stores and does not close or modify them.

All return `DiffResult` with pinned `Before` and `After` descriptors, `Changed`, `RecipeChanged`, and sorted, nonnil `Artifacts`, `Resolutions` and `Declarations` slices. Comparison groups repeated selectors and profiles into sets instead of matching by input index. Artifact identity includes source repository; descriptor annotations and platform hints do not create artifact replacements. Recipe comparison ignores JSON object key order and whitespace while preserving input ordering, large integers and explicit null values.

These APIs validate OCI envelopes, interpreted Bale metadata and selected declaration JSON, including nested bundle metadata. They do not refresh sources, discover referrers, execute plugins, publish content, or read or check the existence of opaque application configs and payloads. Comparison does not prove opaque content availability or integrity. `Inspect`, pull and transfer planning retain complete graph validation.

Detailed observation changes cover each root bundle's metadata. Compare nested bundles separately through their pinned references or `DiffRoots`; their captured artifact membership is still included in the outer inventory. Failure returns an empty result with the underlying typed error and operand context. Cancellation remains detectable with `errors.Is`.

See [snapshot comparison](inspection.md#compare-snapshots) for CLI output, formatting and exit codes.

## Export and apply delivery paths

`client.ExportDelivery(ctx, reference)` resolves a registry bundle once and returns a `DeliveryFile`. It reads and hashes supported OCI manifests and indexes plus interpreted Bale metadata and declaration JSON, validating captured source coordinates, inventory membership, subjects, dependencies and nested native metadata. It does not resolve original sources, discover referrers, or download or check the existence of opaque application configs and payload layers. Remote export does not prove payload availability or integrity; `Inspect`, `Pull`, `PlanPush` and `Push` retain full graph validation.

`ExportDeliveryLayout(ctx, path)` reads a completed local layout without network access or changes to the layout. `ExportDeliveryRoot(ctx, root)` uses a pinned descriptor in a caller-owned store. Both verify the complete captured graph. All three export methods flatten nested native inventories into default delivery paths.

`DeliveryFile.Bundle` binds the file to the root digest. Each `DeliveryArtifact` contains a digest-pinned source `Ref` and a relative destination `Path`. `EncodeDeliveryFile` and `DecodeDeliveryFile` encode and strictly validate its YAML representation. Export produces default paths; retain user edits separately.

Set `CreateOptions.ExportDelivery: true` to receive `CreateResult.Delivery` from that exact capture, including during a dry run. To deliver an edited file, decode it and pass its address as `PushOptions.Delivery` to `PlanPush`. Artifact paths override `RepositoryMap`; omitted artifacts use defaults and remain included. Planning checks the bundle digest and captured identities before publication. Required graph and referrer copies remain colocated even when the primary publication path differs. See [transfers](transfers.md#export-delivery-paths) for the YAML and CLI workflow.

`PlanPush` verifies the complete local graph and builds a sealed in-memory `DeliveryPlan` without network access or writes. `Relocations` reports canonical source-to-destination placements; `Repositories` contains copy roots with their OCI graphs, `Tags` contains restored aliases, and `Destinations` contains generated attachments. `Destination` is the separate bundle completion reference. JSON inspection of a plan is a report and cannot be loaded as an executable plan. Destination availability, credentials and existing remote tags are checked when `Push` executes. The CLI exposes local planning through [`bale push --dry-run`](transfers.md#push-to-a-destination).

## Update delivery paths

```go
updated, err := client.UpdateDeliveryLayouts(ctx, "./locked", "./refreshed", oldFile)
if errors.Is(err, bale.ErrConflict) {
    // updated.Conflicts explains ambiguous assignments; Delivery is nil.
    return err
}
if err != nil {
    return err
}
data, err := bale.EncodeDeliveryFile(*updated.Delivery)
```

`UpdateDelivery(ctx, beforeRef, afterRef, file)` pins each registry operand once, reusing the captured read for identical reference strings. `UpdateDeliveryLayouts(ctx, beforePath, afterPath, file)` reads two local layouts without network access or credential files. `UpdateDeliveryRoots(ctx, beforeRoot, afterRoot, file)` uses caller-owned stores without closing or changing them. None writes the returned file.

All return `DeliveryUpdateResult` with `Before`, `After`, a candidate `Delivery`, and sorted, nonnil `Changes` and `Conflicts`. Exact identities and one-to-one captured selector changes preserve authored paths. Selector sets include all nested inventories and remain separate by input, dependency and plugin role. Omitted old rows make no path claim; unclaimed refreshed artifacts receive default rows. No application-specific matching is performed.

The old file must be bound to the old snapshot and contain only captured identities. Ambiguity returns `ErrConflict` with diagnostics and no candidate `Delivery`; other failures return an empty result and typed error. Read operations and cancellation retain their underlying causes. Caller files and descriptors remain unchanged.

Update validates envelopes, interpreted Bale metadata and selected dependency JSON without refreshing sources, discovering referrers, executing plugins, publishing content, or checking opaque payload availability or integrity. Use `PlanPush` for complete local graph and destination-layout validation. See [delivery updates](delivery-updates.md) for conflict rules, partial files and CLI output.
