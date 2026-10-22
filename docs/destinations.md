# Destination records

Bundle push attaches a destination record to each captured application artifact at its primary delivery location. Consumers can find an artifact's delivered dependencies from the registry without retaining push output or changing the artifact's bytes.

```sh
bale destinations inspect mirror.example.com/release/charts/web:1.4
bale destinations inspect mirror.example.com/release/charts/web:1.4 \
  -f '{{.Local}} {{.Record.Source}} {{.Record.Bundle}}'
```

The default output is an indented JSON array, sorted by attachment digest. Each entry contains `repository`, `subject`, `descriptor`, `record` and `local`. Empty results are `[]`. Inspection validates all candidate metadata before returning output and does not contact the recorded source, bundle or dependency locations. Authentication and exact-host `--plain-http` options work as for other registry commands.

## Record format

Artifact type: `application/vnd.bale.destination.v1+json`. Its OCI manifest has the delivered artifact as its exact `subject`, an empty JSON config and one JSON layer with that artifact type. Manifest and payload are each limited to 4 MiB. Registry JSON rejects unknown fields, duplicates, nulls and trailing data.

Example payload (replace abbreviated digests with complete values):

```json
{
  "schemaVersion": 1,
  "bundle": "mirror.example.com/release@sha256:<bundle>",
  "source": "registry.example.com/charts/web@sha256:<chart>",
  "destination": "mirror.example.com/release/charts/web@sha256:<chart>",
  "resolutions": [
    {
      "capture": "sha256:<owning-bundle>",
      "policy": {"follow": "auto"},
      "outcome": "selected",
      "declaration": {"digest": "sha256:<declaration>", "context": "production"},
      "dependencies": [
        {
          "ref": "registry.example.com/images/web:stable",
          "policy": {"follow": "none"},
          "source": "registry.example.com/images/web@sha256:<image>",
          "destination": "mirror.example.com/release/images/web@sha256:<image>"
        }
      ]
    }
  ]
}
```

`bundle` identifies the delivered bundle by digest. `source` and `destination` identify the same unchanged artifact bytes in their respective repositories. Each resolution retains its owning capture digest, dependency policy and selected declaration, including context. Nested bundles keep separate capture scopes; their resolutions are not combined into a single inferred dependency set.

Dependencies are direct: a chart record lists its declared image destinations, while an image's own dependencies belong in its record. `ref` retains the original selector; `source` pins the captured result and `destination` identifies its confirmed primary placement. Repository identity matters even when digests match. Equivalent entries are deduplicated and metadata has no timestamps or generated IDs.

An `absent` resolution means capture found no matching declaration. `disabled` records explicit opt-out. Both have empty dependency arrays and no declaration. An empty `resolutions` array means no semantic observation was captured for that source identity; it does not establish that the artifact has no dependencies.

## Selecting records

`local` is true when the record's destination matches the repository and digest being inspected. Transferring an artifact again can carry older records alongside the new one; inspection returns them all and chooses no newest record.

Select the intended local record by pinned bundle and source identity, then choose its capture, policy and declaration or context. Reject ambiguous choices. Two source repositories containing identical bytes can produce multiple local records for the same destination, even within one delivery.

The [native Helm integration](helm.md#use-delivered-images-in-helm) reads attachments automatically and applies their pinned image destinations during rendering. Its offline filter also accepts this inspection JSON. [Flux consumption](flux.md) supplies mappings through Flux Operator. Both require one matching local attachment and one selected resolution, rejecting ambiguous choices. The chart archive remains unchanged. [Producer declarations](dependencies.md) determine capture; destination records report the resulting placements and do not participate in dependency selection.

## Publication and limits

Planning creates deterministic records and checks their registry tag reservations before writes. Push copies and verifies the complete artifact and bundle graphs first, then publishes records and confirms their referrer discovery, then publishes aliases and the completion tag. A failure can leave copied content or records behind. Retrying reproduces the same attachment digests and repairs interrupted discovery; publication across repositories is not atomic.

Records cover captured manifest and index identities, including platform manifests and nested bundle roots, at their canonical locations. Extra copies required for OCI graph and referrer colocation receive no additional record. Bale declaration metadata, bundle inventory metadata and destination records themselves receive no generated record. Selected historical records remain ordinary opaque attachments during transfer.

These records describe verified mappings at publication time. Reading one does not prove that its bundle completed, targets remain available, or the publisher is trusted. Recorded locations are JSON metadata, not OCI graph edges or independent garbage-collection roots. Retention and signature verification remain separate concerns; see [registry behavior](registry.md).

Only bundle `Push` creates these records. Direct SDK `Plan`/`Execute` transfers do not have captured dependency resolutions. See the [SDK](sdk.md#inspect-delivered-destinations) for programmatic access.
