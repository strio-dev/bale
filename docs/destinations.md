# Destination records

Bundle push attaches a destination record to each captured application artifact at its primary delivery location. The record lists that subject's captured direct dependencies and their delivered coordinates, with a digest reference to the full bundle. It is not another bundle, and the artifact's bytes remain unchanged.

```sh
bale destinations inspect mirror.example.com/release/charts/web:1.4
```

Use the actual delivered reference. Inspection returns JSON records sorted by attachment digest, with `repository`, `subject`, `descriptor`, `record` and `local`; an empty inventory is `[]`. It validates metadata without contacting recorded source, bundle or dependency locations. See [CLI conventions](reference/cli.md) for authentication and formatting.

## Record format

The artifact type is `application/vnd.bale.destination.v1+json`. Its OCI manifest has the delivered artifact as its exact `subject`, an empty JSON config and one JSON layer with that artifact type. Both manifest and payload are limited to 4 MiB. Registry JSON rejects unknown fields, duplicate keys, nulls and trailing values.

| Payload field | Meaning |
| --- | --- |
| `schemaVersion` | `1`. |
| `bundle` | Delivered bundle reference pinned by digest. |
| `source` | Captured artifact's original repository and digest. |
| `destination` | Same artifact digest at its primary delivered repository. |
| `resolutions` | Captured dependency observations for this source identity. |

Each resolution contains its owning `capture` digest, dependency `policy` and `outcome`. A `selected` outcome includes the chosen `declaration` digest, optional context and direct `dependencies`. Each dependency retains its original `ref`, policy, pinned source and delivered destination.

An `absent` outcome means capture found no matching declaration; `disabled` records explicit opt-out. Both have empty dependency arrays and no declaration. Empty `resolutions` means no semantic observation was captured, rather than proving there are no dependencies.

Nested bundles retain separate capture scopes. A chart record lists its direct image dependencies; an image's own dependencies belong in that image's record. Source repository identity matters even when digests match. Equivalent entries are deduplicated; records contain no timestamps or generated IDs.

## Selecting records

`local` matches the repository and digest being inspected. Older transferred records can coexist with local records; inspection does not choose a newest record.

Select by local destination, pinned bundle and source, then by capture and declaration digest or context. Reject ambiguous matches. Identical bytes from two source repositories can produce multiple local records at one destination, even within a single delivery.

The [Helm integration](helm.md#use-delivered-images-in-helm) reads these attachments and maps rendered image references; its offline filter also accepts inspection JSON. [Flux consumption](flux.md) supplies mappings through Flux Operator. Both require one matching local attachment and one selected resolution. [Producer declarations](dependencies.md) control capture; destination records report the resulting placements.

## Publication and limits

Push verifies complete artifact and bundle graphs, publishes records and confirms referrer discovery, then publishes aliases and the completion tag. Planning checks record and tag reservations before writes. Failures can leave content or records behind; retries reproduce the same record digests and repair interrupted discovery. Publication across repositories is not atomic.

Records cover captured manifests and indexes, including platform manifests and nested bundle roots, at canonical locations. Extra copies needed for graph/referrer colocation receive no extra record. Bale declarations, inventory metadata and destination records themselves receive none. Historical records can still transfer as opaque attachments.

A record describes a verified mapping at publication time. It does not prove bundle completion, continued availability or publisher trust. References inside its JSON are metadata, not OCI graph edges or garbage-collection roots; see [registry behavior](registry.md) for retention.

Only bundle `Push` generates destination records; direct `Plan`/`Execute` lacks captured dependency resolutions. See the [SDK](sdk.md#inspect-delivered-destinations) for inspection, bounded reads and JSON codecs.
