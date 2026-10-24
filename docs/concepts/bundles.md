# How bundles work

A Bale bundle captures OCI artifacts and the content they need. Create the bundle from a recipe, then choose where to publish it at delivery time.

## Recipe, capture and delivery

| Item | Purpose |
| --- | --- |
| Recipe (`bale.yaml`) | Select source references, tags, dependencies, referrers and discovery plugins. |
| Bundle | Lock the artifact graph, recipe and resolved dependencies to immutable digests. |
| Delivery file | Choose publication paths for one captured snapshot. |

Creation resolves mutable tags and locks the full OCI graph, including image platforms and nested bundles. It follows selected dependency declarations and runs configured discovery plugins. Disabling optional referrer discovery or dependency following leaves required OCI descriptor edges intact. Later source-tag changes leave that snapshot unchanged.

A [delivery file](../transfers.md#export-delivery-paths) changes member paths at push time. It cannot add inputs or change captured bytes. Export one during creation or from an existing bundle.

## Three ways to carry the same content

- **Local capture:** default creation keeps manifests and interpreted metadata locally. Payloads remain at their pinned source locations until push or save; discovery may still read charts and plugin packages.
- **Registry:** `create --registry` stores the bundle and its complete graph in one holding repository.
- **Offline copy:** `create --layout` downloads a complete OCI layout. `save` downloads any missing content into a complete archive; `load` restores it without source access.

Save, load and locked transfers preserve the bundle digest. Complete layouts and archives store shared content once by digest; registry deduplication depends on storage and repository scope. A registry bundle holds the required content itself, so later pulls can work without the original sources. Its [retention policy](../registry.md#retention-and-garbage-collection) must preserve the graph.

## Locked and latest

`pull` defaults to **locked**: retrieve the captured graph without resolving source tags or running plugins.

`pull --mode latest` uses the recipe to capture a fresh snapshot from the sources. Add `--allow-plugins` if that recipe uses discovery plugins. [Compare snapshots](../inspection.md#compare-snapshots) and [update delivery paths](../delivery-updates.md) before delivering the refresh.

## Relationships and deployment

OCI referrers attach an artifact to a subject digest. Bale uses them for:

| Record | Purpose |
| --- | --- |
| [Dependency declaration](../dependencies.md) | Name the references to resolve and include during capture. |
| [Destination record](../destinations.md) | Describe captured dependencies at their delivered locations and link to the bundle. |

One matching dependency declaration is followed automatically. Recipes can disable following or select among several declarations. Repeated content is deduplicated.

The [Helm integration](../helm.md) and [Flux adapter](../flux.md) use destination records to apply image mappings while preserving chart bytes. Add deployment-specific image overrides to the recipe explicitly and use their delivered references in deployment values.
