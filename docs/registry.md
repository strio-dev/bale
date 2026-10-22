# Registry behavior

## Authentication and transport

The CLI uses Docker credential configuration, including `DOCKER_CONFIG`. Repeated `--plain-http localhost:5000` permits HTTP only for the named registry. Local layout inspection does not read registry credentials. The [SDK](sdk.md#configure-a-client) uses anonymous authentication unless the caller supplies credentials and accepts caller-controlled HTTP/TLS transport policy.

## OCI graphs and referrer inventories

Bale traverses OCI descriptors and selected referrers. Recognized OCI and Docker manifest or index descriptors in artifact layers are followed recursively; configs and other layer types remain opaque. Generic dependency declarations add references to resolve during bundle creation. For manifests referenced as artifact layers, Distribution 2.8.3 requires the same child bytes to be registered through both manifest and blob endpoints. Bale performs that registration without changing artifact digests.

Each copied OCI object with a subject reserves its exact standard referrer fallback tag (`<algorithm>-<subject digest>`) in every receiving repository, including registries with native referrer discovery. Bale validates existing inventories and their child subjects before writing. Completion tags and member aliases, including aliases captured by `tags: []`, fail publication if they overlap a managed name; captured aliases are never silently excluded. Fallback updates preserve previous inventory manifests.

Publication writes children before the completion tag; multiple tag updates are not atomic. Content reuse, mounts and referrer discovery depend on registry capabilities. Same-client publication serialization and cross-client coordination are described in the [SDK documentation](sdk.md#publication-coordination).

## Retention and garbage collection

Registry retention must preserve untagged manifest registrations in both bundle and delivered member repositories. A completion tag preserves the bundle entry point; the server's cleanup policy decides whether its referenced graph survives.

The following versions and policies were tested with actual GC on nested bundles containing two-platform images, dependency referrers and manifest/index artifacts used as layers. Tests included an unreachable blob control and a fresh pull and push after collection:

| Registry and policy | Nested graph after GC |
| --- | --- |
| Distribution 2.8.3, default GC | Preserved; fresh pull, push and discovery passed |
| Distribution 2.8.3, `--delete-untagged` | Required manifests and payloads removed |
| Zot 2.1.22, GC with default retention | Required artifacts reached through layers and their payloads removed |
| Zot 2.1.22, retention policy with `deleteUntagged: false` | Preserved; fresh pull, push and discovery passed |

For Zot, configure `storage.retention.policies[].deleteUntagged` as `false` in policies covering the bundle and member repositories, and retain their published tags and referrers. Its GC defaults to deleting untagged manifests; layer descriptors do not protect nested manifest registrations like index members do. See [Zot's retention defaults](https://github.com/project-zot/zot/blob/v2.1.22/pkg/retention/retention.go#L51-L74) and [Distribution's untagged deletion behavior](https://github.com/distribution/distribution/blob/v2.8.3/registry/storage/garbagecollect.go#L58-L90).

The Zot tests shortened grace periods and collection intervals to two seconds. Default wall-clock scheduling and other registry versions remain unqualified. Successful publication cannot determine the registry's administrative retention policy.
