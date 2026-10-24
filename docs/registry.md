# Registry access and retention

Configure credentials for transfers and retention for every repository that holds bundle content.

## Authentication and transport

The CLI uses Docker credential configuration, including `DOCKER_CONFIG`. Log in with your usual Docker-compatible registry tool before transfer.

For a local HTTP registry, add `--plain-http localhost:5000`. Repeat the option for each exact host that needs HTTP. Save, load and local inspection work offline and do not load registry credentials.

The [SDK](sdk.md#configure-a-client) defaults to anonymous access; callers supply credentials and configure HTTP/TLS transport.

## OCI graphs and referrer inventories

Bale follows OCI descriptors, selected referrers and [dependency declarations](dependencies.md). It preserves artifact bytes and publishes children before the bundle's completion tag.

Objects with a subject reserve the standard referrer fallback tag (`<algorithm>-<subject digest>`), including on registries with native referrers. Conflicting aliases or completion tags fail rather than replacing that inventory. See [publication behavior](reference/cli.md#publication) for graph traversal, managed tags and concurrent publishers.

## Retention and garbage collection

Preserve untagged manifests in **both bundle and member repositories**. Keeping the completion tag alone does not protect the whole graph from registry cleanup.

These version-specific policies were tested with nested bundles, multiarchitecture images and referrers:

| Registry and policy | Result after GC |
| --- | --- |
| Distribution 2.8.3, default GC | Graph preserved |
| Distribution 2.8.3, `--delete-untagged` | Required manifests and payloads removed |
| Zot 2.1.22, default retention | Required artifacts and payloads removed |
| Zot 2.1.22, `deleteUntagged: false` | Graph preserved |

For Zot, set `storage.retention.policies[].deleteUntagged` to `false` for bundle and member repositories, and retain their tags and referrers. Layer references do not protect nested manifests the same way index membership does. See [Zot's defaults](https://github.com/project-zot/zot/blob/v2.1.22/pkg/retention/retention.go#L51-L74) and [Distribution's untagged deletion](https://github.com/distribution/distribution/blob/v2.8.3/registry/storage/garbagecollect.go#L58-L90).

Check your registry's policy and version before enabling cleanup; the results above do not qualify other versions or default schedules.
