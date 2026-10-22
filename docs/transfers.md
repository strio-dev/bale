# Transfers

Capture artifacts once, then choose where to deliver them. Follow [your first bundle](tutorials/first-bundle.md) for a complete local walkthrough, or read [how bundles work](concepts/bundles.md) for the lifecycle.

The examples below assume an existing [recipe](recipes.md) and registry paths you can access. Replace `registry.example.com` and `mirror.example.com` with your registries.

## Create a bundle

Capture into a new local OCI layout with read access to your sources:

```sh
bale create --layout -f bale.yaml ./release --output-delivery bale.delivery.yaml
```

To store the bundle in a registry instead:

```sh
bale create -f bale.yaml registry.example.com/bundles/release:v1
```

Registry creation places the complete captured graph in the bundle repository. Both forms preserve artifact bytes and digests and run any configured [discovery plugins](plugins.md).

Preview capture without retaining a layout or publishing content:

```sh
bale create --layout -f bale.yaml ./preview --dry-run
```

The preview resolves inputs and runs discovery, then shows pinned artifacts and why they were included. A later create captures again; the preview does not reserve tags or retain content. See [CLI output](reference/cli.md) for JSON and templates.

## Export delivery paths

Use `--output-delivery FILE` during create, including a dry run, to export paths from that exact capture. You can also [export later](inspection.md#export-delivery-yaml) from a stored bundle or layout. The output path must be new.

The generated YAML binds paths to a bundle digest and lists captured manifests and indexes, including dependencies, plugins, attachments and nested inventories. Opaque configs and payload blobs have no editable rows. An abbreviated example:

```yaml
bundle: sha256:<bundle-digest>
artifacts:
  - ref: registry.example.com/charts/web@sha256:<chart-digest>
    path: charts/web
  - ref: ghcr.io/example/web@sha256:<image-digest>
    path: images/web
```

Keep the generated digests and `ref` values; edit only the paths. A path is relative to the destination bundle repository: `images/web` under `mirror.example.com/releases/v1:bundle` becomes `mirror.example.com/releases/v1/images/web`.

```sh
bale push ./release mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml --dry-run
```

Artifact paths override repository-wide `--map` values. Omitted rows use normal mapping and remain included. Unknown or duplicate identities, invalid paths and mismatched bundle digests fail before publication. Required graph and referrer relationships may create additional colocated copies. Path edits do not rewrite references inside chart or application payloads.

Keep authored files separately: exporting again regenerates defaults. After refreshing a bundle, [update the delivery file](delivery-updates.md) to carry path edits forward.

## Locked and latest pulls

```sh
bale pull registry.example.com/bundles/release:v1 ./release
bale pull registry.example.com/bundles/release:v1 ./refreshed --mode latest
```

The default locked pull uses captured digests and needs only the bundle registry when its graph is complete. It neither resolves original selectors nor executes plugins.

Latest pull recaptures the recipe and resolves source, dependency and plugin tags again, even if an input's digest has not changed. Recipes with discovery plugins require explicit execution permission:

```sh
bale pull registry.example.com/bundles/release:v1 ./refreshed \
  --mode latest --allow-plugins
```

Included nested Bale bundles retain their captured graphs in either mode. Pull requires a new layout path. [Compare snapshots](inspection.md#compare-snapshots) before delivering a refresh.

## Push to a destination

Review placement locally, then publish:

```sh
bale push ./release mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml --dry-run
bale push ./release mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml
```

The preview validates the complete local graph and shows artifact locations, required copies, restored tags, destination attachments and the completion tag. It makes no network requests, reads no registry credentials and runs no plugins. Destination access and existing remote tags are checked during actual push.

Without a delivery file, member paths preserve source repository paths below the bundle repository. Map an entire source repository with repeated `--map SOURCE_REPO=RELATIVE_PATH` options:

```sh
bale push ./release mirror.example.com/releases/v1:bundle \
  --map registry.example.com/apps/api=api
```

Push preserves content bytes and publishes [destination records](destinations.md) at primary artifact locations. It then restores aliases and publishes the completion tag. Conflicting repository assignments or existing tags with different digests fail. Multiple tag updates are not atomic; retrying the same push reuses uploaded content and repairs incomplete publication.

The bundle repository retains the complete captured graph. Check [registry retention](registry.md#retention-and-garbage-collection) before enabling cleanup policies.

## Output and errors

See [CLI output and errors](reference/cli.md) for digest output, JSON reports, progress, templates and partial results. A failed push can leave confirmed publications; its completion tag is not a transaction across member repositories.
