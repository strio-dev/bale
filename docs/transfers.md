# Transfer artifacts

Capture artifacts from a [recipe](recipes.md), then choose a destination when you deliver. For a complete local workflow, follow [your first bundle](tutorials/first-bundle.md).

Replace the registry names below with repositories you can access.

## Create a bundle

Capture into a new local OCI layout and export its delivery paths:

```sh
bale create --layout -f bale.yaml ./release --output-delivery bale.delivery.yaml
```

Use new paths for the layout and delivery file. Even with `--layout`, capture requires read access to the source registries. To store the bundle in a holding registry instead:

```sh
bale create -f bale.yaml registry.example.com/bundles/release:v1
```

Registry creation also requires write access to that holding repository; it does not publish members to their delivery paths. Both forms preserve the captured bytes and digests, include the complete graph, and run configured [discovery plugins](plugins.md).

Preview the capture first with:

```sh
bale create --layout -f bale.yaml ./preview --dry-run
```

The preview reads source registries, resolves tags and runs configured discovery plugins without retaining or publishing a bundle. A later create resolves inputs again. Without `--layout`, a create preview also checks the holding registry for tag conflicts.

## Export delivery paths

Use `--output-delivery FILE` during create, including a dry run, or [export later](inspection.md#export-delivery-yaml). The file names captured artifacts and their paths under the destination bundle repository:

```yaml
bundle: sha256:<bundle-digest>
artifacts:
  - ref: registry.example.com/charts/web@sha256:<chart-digest>
    path: charts/web
  - ref: ghcr.io/example/web@sha256:<image-digest>
    path: images/web
```

Edit only `path` values. For example, `images/web` under `mirror.example.com/releases/v1:bundle` publishes to `mirror.example.com/releases/v1/images/web`.

Omitted rows remain included with default mapping. Bale may make additional copies to keep related OCI content together. Paths control publication locations; they leave chart and application content unchanged. See [delivery file rules](reference/cli.md#delivery-files) for identity, validation and mapping precedence.

Keep your edited file: exporting again generates defaults. [Update it](delivery-updates.md) when you refresh the bundle.

## Locked and latest pulls

```sh
bale pull registry.example.com/bundles/release:v1 ./release
bale pull registry.example.com/bundles/release:v1 ./refreshed --mode latest
```

The default **locked** pull retrieves captured digests without running plugins. **Latest** recaptures the recipe from its sources and resolves source, dependency and plugin tags again, even when an input digest is unchanged. Permit configured plugins explicitly:

```sh
bale pull registry.example.com/bundles/release:v1 ./refreshed \
  --mode latest --allow-plugins
```

Nested Bale bundles retain their captured graphs in either mode. Pull requires a new layout path. [Compare snapshots](inspection.md#compare-snapshots) before delivering the refresh.

## Push to a destination

Review placement locally, then publish:

```sh
bale push ./release mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml --dry-run
bale push ./release mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml
```

The preview validates the local graph and shows publication locations without network access, credentials or plugin execution. Actual push checks destination access and existing tags.

Without a delivery file, source repository paths are preserved beneath the bundle repository, omitting the source registry hostname. For example, `ghcr.io/example/web` maps beneath `mirror.example.com/releases/v1:bundle` to `mirror.example.com/releases/v1/example/web`. Map a whole repository with repeatable `--map` options:

```sh
bale push ./release mirror.example.com/releases/v1:bundle \
  --map registry.example.com/apps/api=api
```

Push publishes content, [destination records](destinations.md) and aliases, then the completion tag. Tag updates across repositories are not atomic. Retry the same push to reuse uploaded content and repair incomplete publication. Configure [retention](registry.md#retention-and-garbage-collection) for both bundle and member repositories.

## Output and errors

See [CLI output](reference/cli.md) for JSON, templates and partial reports, and [publication behavior](reference/cli.md#publication) for conflicts and retries.
