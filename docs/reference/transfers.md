# Transfer behavior

Validation and matching rules for [transfers](../transfers.md), [inspection](../inspection.md) and [delivery updates](../delivery-updates.md).

## Delivery files

A delivery file binds to one bundle digest. Rows identify manifests and indexes by source repository and digest, including dependencies, plugins, attachments and nested inventories. Opaque configs and payload blobs have no editable rows.

Explicit paths override repository-wide `--map` settings; omitted rows use normal mapping. Unknown or duplicate identities, invalid paths and mismatched bundle digests fail before publication. Required graph/referrer relationships can create additional colocated copies.

Exports require a new file and regenerate defaults. Authored paths are separate from bundle content.

## Matching delivery paths

Update matches both snapshots, including nested inventories:

- Exact repository/digest matches retain explicit paths.
- Input, dependency and plugin selectors carry paths to changed digests only when both resolution sets identify one artifact.
- Repeated aliases deduplicate. Several old artifacts can converge if their paths agree.
- Unclaimed artifacts receive defaults; removed artifacts are reported.

Old rows are explicit claims, including default-looking paths; omitted rows make no claim. Update does not carry forward `--map` settings.

Selectors are grouped by role and full reference, not input position. Different nested captures can make one ambiguous. Platform children, declarations and arbitrary referrers without selector evidence receive defaults.

Conflicting paths or multiple selector resolutions prevent output. An artifact that remains while its tag moves elsewhere can also receive competing claims.

## Inspection and comparison

Inspection and local delivery export validate the complete graph. Diff and registry export read manifests and interpreted metadata without checking opaque payloads. None refresh selectors or run plugins.

Each registry reference resolves once, so comparing a mutable tag with itself uses one pinned snapshot. Repeated selectors are compared as sets. Detailed selector/declaration changes describe root bundles; nested inventory membership is included. Compare nested bundles separately for their details. Artifact identity is repository plus digest; opaque blobs are excluded. A tag moving to already-captured content still produces a selector change.

## Update reports

JSON reports contain `before`, `after`, `changes`, `conflicts` and, on success, `delivery`. Conflicts return 125 and a report without writing a file. Invalid inputs and read/template failures return 125 without a report.

## Publication

Bale traverses OCI/Docker manifests and indexes referenced as artifact layers; configs and other layers stay opaque. Distribution 2.8.3 requires manifest-layer content registered through both manifest and blob endpoints; Bale preserves its digest while doing so.

Subject artifacts reserve the fallback tag in each receiving repository. Bale validates inventories and child subjects, preserves prior inventory manifests, and rejects conflicting aliases/completion tags, including aliases captured with `tags: []`.

Push publishes children before the completion tag. Existing tags with different digests and conflicting repository assignments fail. Updates across repositories are not atomic: failure can leave confirmed publications, and retrying reuses content and repairs incomplete publication. See [publication coordination](../sdk.md#publication-coordination) for concurrent clients.
