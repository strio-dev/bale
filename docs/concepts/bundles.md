# How bundles work

A Bale bundle captures OCI artifacts and the content they need. Its recipe describes the inputs; its captured graph fixes their digests; delivery chooses where to publish them.

## Recipe, capture and delivery

| Item | Purpose |
| --- | --- |
| Recipe (`bale.yaml`) | Select source references, tags, referrers, dependencies and discovery plugins. |
| Bundle | Retain the resolved manifests and blobs, recipe and dependency observations. |
| Delivery file | Set publication paths for the artifacts in one captured snapshot. |

Creation resolves mutable tags and includes the full OCI graph, including image platforms. It follows selected dependency declarations and runs configured discovery plugins. The resulting bundle uses digest references, so later source-tag changes do not alter it.

The destination is chosen at push time. A [delivery file](../transfers.md#export-delivery-paths) lets you edit member paths before pushing; it cannot add inputs or change captured bytes. Export one during creation or from an existing bundle.

## Three ways to carry the same content

- **Registry:** publish a bundle root and its complete graph into a holding repository. Pull it later for transfer.
- **Local OCI layout:** create or pull a bundle into a directory with manifests and blobs addressed by digest.
- **Archive:** save the layout as a tar file and restore it with `load` on the receiving side.

Save, load and locked transfers preserve the bundle digest. Content shared within a layout or archive is stored once by digest. Registry deduplication depends on its storage implementation and repository scope. Bale copies the required content into the holding repository so capture does not depend on remote source references remaining available.

Registry retention must preserve the graph. JSON references alone do not protect content from garbage collection; see [registry behavior](../registry.md).

## Locked and latest

`pull` defaults to **locked**: retrieve the captured graph without resolving source tags or executing plugins.

`pull --mode latest` uses the captured recipe to resolve inputs again and creates a new snapshot. It needs access to the sources. If that recipe uses plugins, add `--allow-plugins` to permit their execution. [Compare the snapshots](../inspection.md#compare-snapshots) and [update delivery paths](../delivery-updates.md) before delivering the refreshed bundle.

## Relationships and deployment

OCI **referrers** attach artifacts to a subject digest. Bale uses that mechanism for two different records:

| Record | When used | Contains |
| --- | --- | --- |
| [Dependency declaration](../dependencies.md) | During capture | References the subject needs, which Bale resolves and includes. |
| [Destination record](../destinations.md) | After delivery | The subject's captured dependencies at their delivered locations, plus a reference to the full bundle. |

Selecting an artifact follows one matching dependency declaration automatically; recipes can disable this or select among multiple declarations. Repeated content is deduplicated.

Bale preserves artifact bytes. Consumers use destination records to find dependencies at their new locations. The [Helm integration](../helm.md) and [Flux adapter](../flux.md) apply those mappings to chart-default images. Deployment-specific image overrides are explicit recipe inputs and destination deployment values.
