# Registry behavior

Configure access and retention for every repository that holds bundle content. Registry policies determine whether published content remains available.

## Authentication and transport

The CLI uses Docker credential configuration, including `DOCKER_CONFIG`. Authenticate with your usual Docker-compatible registry tooling before transfer. Add `--plain-http localhost:5000` for a local HTTP registry; repeat it for each exact host that requires HTTP.

Local layout operations do not load registry credentials. The [SDK](sdk.md#configure-a-client) defaults to anonymous authentication; callers supply credentials and configure HTTP/TLS transport.

## OCI graphs and referrer inventories

Bale follows OCI descriptors and selected referrers. OCI and Docker manifests or indexes referenced as artifact layers are traversed; configs and other layers stay opaque. [Dependency declarations](dependencies.md) add source references during capture. Distribution 2.8.3 requires manifest-layer content registered through both manifest and blob endpoints; Bale does this without changing digests.

Objects with a subject reserve the standard fallback tag (`<algorithm>-<subject digest>`) in every receiving repository, including registries with native referrers. Bale validates existing inventories and child subjects, preserves prior inventory manifests, and rejects aliases or completion tags that collide with managed names. This includes aliases captured with `tags: []`; none are silently dropped.

Children are published before the completion tag. Tag updates are not atomic across repositories, and content reuse, mounts and referrer discovery depend on registry capabilities. See [SDK publication coordination](sdk.md#publication-coordination) for concurrent clients.

## Retention and garbage collection

Preserve untagged manifest registrations in **both bundle and member repositories**. Keeping the completion tag alone does not guarantee that registry cleanup retains every referenced manifest.

The following policies were tested with actual garbage collection of nested bundles, two-platform images, dependency referrers and manifest/index artifacts used as layers. Each test checked an unreachable blob control and attempted a fresh pull and push afterward.

| Registry and policy | Result after GC |
| --- | --- |
| Distribution 2.8.3, default GC | Graph preserved; pull, push and discovery passed |
| Distribution 2.8.3, `--delete-untagged` | Required manifests and payloads removed |
| Zot 2.1.22, default retention | Required artifacts reached through layers and their payloads removed |
| Zot 2.1.22, `deleteUntagged: false` retention policy | Graph preserved; pull, push and discovery passed |

For Zot, set `storage.retention.policies[].deleteUntagged` to `false` in policies covering bundle and member repositories, and retain their tags and referrers. Its default GC deletes untagged manifests; layer descriptors do not protect nested manifest registrations as index members do. See [Zot's defaults](https://github.com/project-zot/zot/blob/v2.1.22/pkg/retention/retention.go#L51-L74) and [Distribution's untagged deletion](https://github.com/distribution/distribution/blob/v2.8.3/registry/storage/garbagecollect.go#L58-L90).

Zot tests used two-second grace periods and collection intervals. Other versions and default scheduling have not been qualified; successful publication does not establish a registry's administrative retention policy.
