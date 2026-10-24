# Go SDK

Import `github.com/strio-dev/bale` to capture bundles, transfer OCI graphs and inspect delivery metadata. Application integrations supply discovery and reference rewriting; Bale preserves artifact bytes. See [bundle lifecycle](concepts/bundles.md) for the underlying model.

## Configure a client

```go
client, err := bale.NewClient(bale.ClientOptions{})
if err != nil {
    return err
}
```

The examples below are function-body excerpts using this client and an existing `context.Context` named `ctx`, in a function returning `error`. Transfer and attachment examples also import `fmt`. Registry names are placeholders.

| `ClientOptions` field | Purpose |
| --- | --- |
| `Credentials` | Caller-supplied ORAS credential callback. The default is anonymous; the SDK does not read Docker credential files. |
| `HTTPClient` | Caller-owned TLS and transport policy. |
| `PlainHTTPRegistries` | Exact registry hosts allowed to use HTTP. |
| `OnProgress` | Synchronous, serialized progress callbacks. Return promptly. |
| `PluginRunner` | Explicit support for running configured discovery plugins. |

Capture progress reports resolution, plugin fetch/run and capture phases; `captured` precedes publication. Recipe events identify the zero-based `Input`, `Reference` and `Plugin`, with a pinned `Descriptor` when available. Capture notifications are advisory and can be skipped while another callback is active. Determine completion from results, not event counts.

## Capture and archive locally

Import `github.com/strio-dev/bale/store` as `store`. The managed store shares blobs across retained bundle IDs and optional local names:

```go
local, err := store.Open(client, store.Options{})
if err != nil {
    return err
}
recipe, err := bale.ParseRecipe([]byte(`include:
  - registry.example.com/apps/web:stable
`))
if err != nil {
    return err
}
captured, err := local.Create(ctx, recipe, store.CreateOptions{
    Tags:           []string{"release:v1"},
    ExportDelivery: true,
})
if err != nil {
    return err
}
_, err = local.Save(ctx, captured.Descriptor.Digest.String(), "./release.tar")
if err != nil {
    return err
}
_, err = local.Load(ctx, "./release.tar", []string{"restored:v1"})
return err
```

`store.Options.Path` selects a store. An empty path uses `$XDG_DATA_HOME/bale`, or `~/.local/share/bale`. `ParseRecipe` reads strict authoring YAML; `NormalizeRecipe` validates and applies defaults to a deep copy of a Go recipe.

`local.Create` needs only source read access. It reports `MetadataOnly: true` while required payloads are missing from the shared store. It keeps manifests and interpreted metadata without downloading ordinary image payloads; discovery may read chart and plugin bytes. `store.CreateOptions.DryRun` retains no bundle; `ExportDelivery` returns paths from that exact capture. Managed results leave `LayoutPath` empty and do not publish to registries.

`local.Save` hydrates missing payloads into the shared store from pinned source digests, then exports a complete, deduplicated OCI-layout tar archive. Supply credentials and transport options on the client. The bundle digest is unchanged; subsequent local inspection reports complete content. Save of a complete stored bundle and `local.Load` are offline. Archive output paths must be new.

| Operation | Managed store API |
| --- | --- |
| Capture metadata | `local.Create(ctx, recipe, options)` |
| Retrieve a bundle | `local.Pull(ctx, reference, store.PullOptions{Mode: bale.PullLocked})` |
| Archive / import | `local.Save(ctx, name, archive)` / `local.Load(ctx, archive, tags)` |
| Inspect / compare | `local.Inspect(ctx, name)` / `local.Diff(ctx, before, after)` |
| Preview / deliver | `local.PlanPush(ctx, name, destination, options)` / `local.Push(ctx, name, destination, options)` |

`local.PlanPush` returns a preview rather than an executable `bale.Client.Push` plan. `List`, `Tag` and `Remove` manage retained roots and names; pruning shared content is explicit.

The client also exposes explicit directory APIs: `CreateLocal` writes a source-dependent metadata layout, while `CreateLayout` downloads a complete offline layout. `Save`, `Load`, `InspectLayout` and `PlanPush` take these standalone paths. Registry `Create` stores a complete bundle in a holding repository. These lower-level APIs preserve the same OCI bundle format.

## Transfer an artifact graph

Direct transfers do not require a bundle:

```go
root, err := client.Resolve(ctx, "registry.example.com/components/tool:v1")
if err != nil {
    return err
}
plan, err := client.Plan(ctx, bale.TransferRequest{
    Roots:       []bale.Root{root},
    Destination: "mirror.example.com/release",
    RepositoryMap: map[string]string{
        root.Repository: "components/tool",
    },
})
if err != nil {
    return err
}
result, err := client.Execute(ctx, plan)
fmt.Printf("Confirmed relocations: %d\n", len(result.Relocations))
return err
```

`Plan` validates the graph without publishing; execution uses pinned descriptors. `plan.View()` is a defensive inspection copy, not a serializable executable plan. `result.Relocations.Lookup(bale.ArtifactID{Repository: root.Repository, Digest: root.Descriptor.Digest})` returns a confirmed destination coordinate. Tags are reported separately; publication across tags and repositories is not atomic.

### Caller-owned content and discovery

Construct `Root` with `Repository` provenance, `Descriptor`, caller-owned `content.ReadOnlyStorage`, and optional `Aliases`. Keep stores available through execution. Bale neither closes them nor takes ownership; callers coordinate shared-store access and preserve digest-addressed content integrity.

The default referrer policy is `all` and requires `ReferrerSource`; `Resolve` supplies a registry provider. For a store without discovery, set `Referrers: bale.ReferrerPolicy{Include: "none"}`. Required referrers remain additive. Caller-owned providers choose their listing strategy; Bale verifies selected attachment bytes and subjects.

Direct `Plan`/`Execute` follows the immutable OCI graph and selected referrers, without resolving references inside dependency declarations. Use bundle creation to capture those dependencies. Bundle push also generates [destination records](destinations.md).

## Author dependency declarations

```go
subject, err := client.Resolve(ctx, "registry.example.com/charts/web:1.4")
if err != nil {
    return err
}
request, err := bale.PrepareDependencyDeclaration(ctx, subject,
    bale.DependencyDeclaration{Dependencies: []bale.DependencyReference{
        {Ref: "registry.example.com/images/web:stable"},
    }})
if err != nil {
    return err
}
attached, err := client.Attach(ctx, request)
fmt.Printf("Published: %t, discoverable: %t\n", attached.Published, attached.Discoverable)
return err
```

Preparation builds deterministic local metadata without reading the subject store or resolving dependency targets. `ParseDependencyDeclaration` accepts strict authoring YAML with an implicit version; `EncodeDependencyDeclaration` and `DecodeDependencyDeclaration` validate registry JSON with `schemaVersion: 1`. See [dependency declarations](dependencies.md) for fields and following rules.

### Run a dependency plugin

Import `github.com/strio-dev/bale/plugin` as `plugin` and opt into recipe execution:

```go
client, err := bale.NewClient(bale.ClientOptions{
    PluginRunner: plugin.Runner(plugin.Options{}),
})
if err != nil {
    return err
}
```

Creation resolves OCI plugin packages, verifies and extracts their modules, binds inputs and retains provenance plus generated declarations. Without a runner, a recipe requiring execution fails. Locked pull and push never run plugins. Latest refresh additionally requires `PullWithOptions(ctx, ref, path, bale.PullLatest, bale.PullOptions{AllowPlugins: true})`.

| Plugin API | Use |
| --- | --- |
| `plugin.RunOCI(ctx, client, request, options)` | Run an OCI plugin using the client's credentials and transport. |
| `plugin.Run(ctx, request, options)` | Run an explicit local module. |
| `plugin.Pack(ctx, store, packagePath)` | Validate a plugin `.tgz` and write its unchanged archive into a transferable OCI graph. |

Manual requests supply plugin-defined JSON, including any required archive bytes. Recipe blob bindings apply during capture. Defaults are 30 seconds and 256 MiB of linear memory; an empty `CacheDirectory` disables persistent caching. Keep cached native code local and trusted. Runs serialize within the process and waits are cancellable; synchronous compilation can delay cancellation. These settings do not cap compilation time or total process memory. See [dependency plugins](plugins.md) for the portable contract and limits.

## Publish and discover attachments

`Attach(ctx, AttachRequest{Subject: reference, Descriptor: descriptor, Store: store})` publishes an OCI manifest or index into its existing subject's repository. Supply matching `subject` metadata and the complete strong dependency closure. Bale preserves bytes, verifies uploaded content and confirms discoverability. Retrying repairs missing inventory entries even when the manifest already exists.

| API | Result |
| --- | --- |
| `ListReferrers(ctx, ref, options)` | Sorted, verified descriptors; optional `ArtifactType` filter. An error may retain already validated entries. |
| `InspectDependencies(ctx, ref)` | Parsed declarations sorted by digest, without selecting or resolving their dependencies. Errors return no declarations. |
| `Attach(ctx, request)` | Separate confirmed `Published` and `Discoverable` states, including on failure. |

Referrer filters apply after validating envelopes and subjects. Without an explicit artifact type, an OCI manifest uses its config media type. Graph upload statistics can undercount bytes when content upload succeeds but inventory publication fails; `Published` relies on independent verification.

### Inspect delivered destinations

`InspectDestinations(ctx, ref)` returns sorted `InspectedDestination` records with `Repository`, pinned `Subject`, attachment `Descriptor`, parsed `Record` and `Local`. It reads metadata without contacting recorded locations. Empty results are nonnil slices; errors return no records.

`InspectDestinationsWithOptions` accepts `DestinationInspectionOptions.MaxMetadataBytes` and `MaxReferrers` to bound aggregate metadata reads and advertised inventory entries. The byte budget includes validation rereads; zero preserves default behavior. These limits do not cap process memory.

`Local` matches the queried repository and digest. Select the intended source, bundle, capture and declaration explicitly and reject ambiguity; local does not establish recency or completion. `DeliveryPlan.Destinations` contains planned attachments; `PushResult.Destinations` reports their publication and discovery separately. `EncodeDestinationRecord` and `DecodeDestinationRecord` validate the JSON wire format. See [destination records](destinations.md).

### Publication coordination

`Attach`, `Create`, `Execute` and `Push` serialize registry publication per client, with cancellable waits. Reads and dry runs do not acquire that permit. Applications coordinate overlapping writers using other clients or processes; fallback inventories offer no distributed atomicity. `Stats` counts graph work rather than total HTTP traffic: downloads exclude verification/latest rereads, uploads include generated JSON, and API overhead is excluded. See [registry behavior](registry.md).

## Inspect a bundle

| Operation | Managed store | Registry client | Explicit layout | Caller-owned root |
| --- | --- | --- | --- | --- |
| Inspect | `local.Inspect` | `Inspect` | `InspectLayout` | `InspectRoot` |
| Compare | `local.Diff` | `Diff` | `DiffLayouts` | `DiffRoots` |
| Export delivery paths | `local.ExportDelivery` | `ExportDelivery` | `ExportDeliveryLayout` | `ExportDeliveryRoot` |
| Carry edited paths forward | `local.UpdateDelivery` | `UpdateDelivery` | `UpdateDeliveryLayouts` | `UpdateDeliveryRoots` |

Managed methods resolve local IDs or names. Registry variants pin each reference once; identical operands share a read in comparison and updates. Local metadata operations are offline. Root variants use pinned descriptors without closing or modifying caller-owned stores.

`InspectLayout` recognizes marked metadata captures and returns `MetadataOnly: true` after verifying their local manifests and interpreted metadata, including nested bundles. It makes no source requests and does not prove missing payload availability or integrity. Complete layouts, registry inspection and caller-owned roots receive full graph verification, which can hash substantial payloads. Reports include `Descriptor`, `Metadata` and selected `Declarations`. Metadata retains recipes, roots, origins, referrers, dependency resolutions and plugin provenance. Inspect a nested bundle separately with its descriptor and the same store. Failure returns an empty report and typed error; integrity verification does not verify publisher signatures.

## Compare bundles

`DiffResult` contains pinned `Before`/`After`, `Changed`, `RecipeChanged`, and sorted, nonnil `Artifacts`, `Resolutions` and `Declarations`. Artifact identity includes source repository and digest. Repeated selectors are compared as sets; recipe comparison preserves input order and JSON values while ignoring object key order and whitespace.

Diff validates envelopes and interpreted metadata, including nested metadata, without reading opaque application payloads. It does not prove their availability or integrity. Detailed observations cover the root bundle; compare nested observations separately. Failure returns an empty result with operand context; cancellation remains detectable with `errors.Is`. See [snapshot comparison](inspection.md#compare-snapshots).

## Export and apply delivery paths

Export returns a `DeliveryFile` bound to the bundle digest. Its `Artifacts` contain source `Ref` and relative destination `Path`, flattening nested inventories. `EncodeDeliveryFile` and `DecodeDeliveryFile` handle strict YAML. Registry export and export from a marked metadata capture validate manifests and interpreted metadata without checking opaque payloads; complete layouts and caller-owned roots receive full graph verification.

Pass an edited file as `PushOptions.Delivery` to `PlanPush`. Paths override `RepositoryMap`; omitted rows retain defaults and remain included. Planning verifies local metadata, digest binding, captured identities and destination layout without network access or writes. Complete layouts also receive full payload verification. Its sealed, in-memory `DeliveryPlan` reports `MetadataOnly` and planned `Relocations`, `Repositories`, `Tags` and `Destinations`; `Destination` is the bundle completion reference. The plan binds the capture's pinned source locations and rejects locator changes before execution. JSON output cannot be reloaded as an executable plan. `Push` reads missing payloads by pinned digest, reuses destination content or registry mounts when possible, and verifies streamed uploads. It checks source and destination access and existing tags without resolving mutable source tags or rerunning plugins. Its result retains confirmed `Relocations` and `Tags` on failure; `Repositories`, `RepositoryMap` and `Referrers` describe planned work. See [delivery paths](transfers.md#export-delivery-paths).

## Update delivery paths

Update methods return a candidate file; they do not write it. `DeliveryUpdateResult` contains `Before`, `After`, `Delivery`, sorted `Changes` and `Conflicts`. Preserve authored paths for exact identities or unambiguous one-to-one selector changes, including nested inventories. New, unclaimed artifacts receive default paths; omitted old rows make no path claim.

The old file must match the old snapshot and contain only captured identities. Ambiguity returns `ErrConflict` with diagnostics and no candidate `Delivery`; other failures return an empty result. Update validates metadata without reading opaque payloads or refreshing sources. Use `PlanPush` to validate local content and publication paths; metadata captures still depend on their pinned sources during `Push`. See [delivery updates](delivery-updates.md) for matching and conflict rules.
