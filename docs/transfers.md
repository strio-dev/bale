# Transfers

## Create a bundle

Capture directly into a new local OCI layout when you only have read access to sources:

```sh
bale create --layout -f bale.yaml ./release --output-delivery bale.delivery.yaml
bale create --layout -f bale.yaml ./release --dry-run
```

Local creation publishes nothing to a registry. Save and load the layout as a portable archive using the [offline packaging walkthrough](offline.md). The following registry-backed workflow remains available:

```sh
bale create -f bale.yaml registry.example.com/bundles/release:v1
bale create -f bale.yaml registry.example.com/bundles/release:v1 --dry-run
bale create -f bale.yaml registry.example.com/bundles/release:v1 --dry-run --format json
```

Creation stores the complete captured graph in the chosen bundle repository. A dry run resolves the inventory without publishing. Configured [discovery plugins](plugins.md) run in both modes; their graphs and generated declarations become part of the capture without publishing source attachments. A bundle is an OCI index pointing to its captured content and JSON inventory; original image, chart and other artifact digests remain unchanged.

The default dry-run report shows the prospective bundle digest and an artifact inventory with full pinned references, original selectors and inclusion reasons. Roles identify selected inputs, discovered dependencies, plugins, attachments and remaining OCI graph content, including platform manifests. Rows deduplicate each repository/digest pair; one artifact can have several roles. The inventory lists manifests and indexes, excluding opaque config and payload blobs.

Use `--format json` for the complete operation report or a Go template such as `--format '{{.Descriptor.Digest}}'` for only the prospective digest. The preview uses the captured metadata without resolving selectors again.

## Export delivery paths

Creation can also write the exact captured inventory to a new YAML file:

```sh
bale create -f bale.yaml registry.example.com/bundles/release:v1 \
  --output-delivery bale.delivery.yaml
```

This uses the same capture that creates the bundle, including for `--dry-run`; it does not resolve tags a second time. A dry run does not retain the captured content for later publication.

You can instead [export the file later](inspection.md#export-delivery-yaml) from the stored bundle or a pulled layout. Export regenerates default paths; save edited files separately. Output files must not already exist, protecting recipes and previous edits.

Registry export validates manifests, indexes and interpreted Bale metadata, including nested inventories, without downloading or checking opaque application configs or payload layers. It does not prove payload availability or integrity. Ordinary inspection, pull, local export and push planning still validate the complete captured graph.

The YAML binds delivery choices to a bundle digest and lists its captured manifest and index artifacts, including discovered dependencies, plugin graphs, attachments, platform manifests and nested bundle inventories. It excludes opaque config and payload blobs. For example (replace the abbreviated digests with complete values):

```yaml
bundle: sha256:<bundle-digest>
artifacts:
  - ref: registry.example.com/charts/web@sha256:<chart-digest>
    path: charts/web
  - ref: ghcr.io/example/web@sha256:<image-digest>
    path: images/web
```

The recipe chooses what to capture; the delivery file chooses artifact placement. Edit `path` values, then pass the file to push:

```sh
bale push ./release mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml
```

Paths are relative to the destination bundle repository: `images/web` becomes `mirror.example.com/releases/v1/images/web`. Captured tags are restored there. The destination registry remains a push-time choice.

Artifact paths override repository-wide `--map` values. Omitting a row uses the normal mapping; deleting rows does not remove content. Bale rejects unknown or duplicate identities, invalid paths and a bundle digest that does not match the layout before publication. Changing `ref` cannot replace captured bytes. Mandatory graph and attachment relationships can require additional copies in colocated repositories; path choices set primary publication locations without removing those required copies. Bale preserves artifact payload bytes and does not rewrite references inside charts or other application formats.

## Locked and latest pulls

```sh
bale pull registry.example.com/bundles/release:v1 ./release
bale pull registry.example.com/bundles/release:v1 ./refreshed --mode latest
```

Locked pull is the default. It reads the captured graph without discovering new declarations or resolving dependency tags. The original source registries are not needed when the stored bundle graph is complete. Plugins are retained as content and are not executed.

For a recipe containing plugins, authorize execution explicitly:

```sh
bale pull registry.example.com/bundles/release:v1 ./refreshed --mode latest --allow-plugins
```

Latest pull recaptures the recipe, reruns configured discovery and resolves its source, plugin and dependency tags into a new local snapshot, including when the parent artifact digest has not changed. An included nested Bale bundle retains its existing captured graph in either mode. If a latest pull changes the bundle digest, [update the delivery file](delivery-updates.md) using the old and refreshed snapshots to carry unambiguous path edits forward. Exporting a fresh file instead regenerates defaults.

Pull creates a new OCI layout and refuses an existing destination path. Use [inspection](inspection.md) to read its captured inventory.

## Push to a destination

Preview delivery before uploading:

```sh
bale push ./release mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml --dry-run
bale push ./release mirror.example.com/releases/v1:bundle \
  --dry-run --format json
```

The readable preview shows canonical source-to-destination paths, copy roots and their complete OCI graphs, restored tags, generated destination attachments and the completion tag. Additional copies preserve graph edges and referrer colocation even when a delivery path chooses a different primary location. The bundle repository retains the complete captured graph.

Dry run uses the same `PlanPush` validation as a real push: it verifies the complete local content and checks delivery-file identities, repository mappings and local tag reservations. It leaves the layout unchanged, performs no network requests, reads no registry credentials and runs no plugins. Destination availability, authentication and conflicts with existing remote tags are checked during actual push. The preview reserves nothing in the registry.

Use `--format json` for the existing delivery plan or a Go template such as `--format '{{json .Relocations}}'`. `--quiet` retains the report. JSON is a report; it is not a saved executable plan. A later push validates and plans the layout again.

Publish after reviewing:

```sh
bale push ./release mirror.example.com/releases/v1:bundle
bale push ./release mirror.example.com/releases/v1:bundle \
  --map registry.example.com/apps/api=api
```

Push transfers captured content without executing plugins. The destination registry is selected at push time. Member repositories default to paths below the destination bundle repository, preserving source repository paths. Repeat `--map SOURCE_REPO=RELATIVE_PATH` to choose relative paths. Repository collisions and existing tags with different digests fail publication.

After verifying the artifact and bundle graphs, push publishes [destination records](destinations.md) at primary artifact locations. These let later consumers find the captured dependencies and their delivered coordinates through `bale destinations inspect SUBJECT`. Aliases and the completion tag are published afterward. Multiple tag updates are not atomic. Content reuse, cross-repository mounts, referrer discovery and retention depend on the registry; see [registry behavior](registry.md).

## Output and errors

`create`, `pull` and `push` print the completed bundle digest. `create --dry-run` defaults to an inventory preview; `push --dry-run` defaults to a delivery preview. Explicit `--format json` returns the full operation report or dry-run plan, and `--format '{{.Descriptor.Digest}}'` applies a Go template in either mode. `create -f` selects the recipe file.

Progress goes to stderr and identifies the recipe input while resolving artifacts, fetching plugins, discovering dependencies and capturing content. `--quiet/-q` suppresses progress while retaining the chosen stdout result, including a dry-run preview. Capture errors identify the failed zero-based `include` index, source reference, pinned subject digest when available and configured plugin. Underlying registry, binding and guest diagnostics remain in the error.

Use `bale help COMMAND` or `bale COMMAND --help` for command-specific options. Failures print `Error:` to stderr and exit with status 125. With explicit `--format json`, a failed `create`, `pull` or `push` also retains an available operation report on stdout, describing confirmed work and partial progress. Exit status 125 remains authoritative: a reported digest does not mean the transfer completed. Default digest output and Go templates stay empty on failure, as do dry-run errors and failures before an operation report exists. [Batch inspection](inspection.md) preserves successful reports when some inputs fail.

A failed push report includes attempted coordinates as context. Its `relocations` and `tags` contain confirmed publications; each destination attachment has separate `published` and `discoverable` flags. Retry the same push to reuse available content and repair incomplete publication. Selected referrer manifests can be published again to restore discovery.
