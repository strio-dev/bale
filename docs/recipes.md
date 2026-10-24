# Recipe syntax

A recipe selects what to capture. Write it in YAML; Bale stores the normalized recipe and captured inventory as JSON. Delivery paths belong in a separate [delivery file](transfers.md#export-delivery-paths), so choosing a destination does not require capturing again.

## Select references and tags

```yaml
include:
  - registry.example.com/apps/api:stable
  - ref: registry.example.com/apps/database:17
    as: database
  - repository: registry.example.com/apps/worker
    tags: ["1.4", stable]
  - repository: registry.example.com/shared/tools
    tags: []
```

| Field | Meaning |
| --- | --- |
| `include` | Required, nonempty list of inputs. A reference string is shorthand for `{ref: ...}`. |
| `ref` | One artifact with an explicit registry and tag or digest. |
| `repository` + `tags` | Repository without a tag/digest, plus required tag list. Use this instead of `ref`. |
| `tags: []` | Capture all tags observed at creation. Nonempty lists select literal tags; `[all]` selects the tag named `all`. |
| `as` | Optional unique display label for the input in capture previews. It does not set a delivery path or registry tag. |
| `referrers` | Attachment inclusion policy; defaults to `include: all`, `require: []`. |
| `dependencies` | Declaration-following policy; defaults to `follow: auto`. |
| `plugin` | Optional OCI discovery plugin and its input. |

Creation pins selected digests and preserves complete multiarchitecture graphs. Sources need an explicit registry host: Bale does not assume Docker Hub or a `latest` tag. Recipes contain exactly one YAML document. Unknown and duplicate fields, aliases, anchors and YAML merge keys are rejected. Outside `plugin.input`, scalar values must be nonempty strings without surrounding whitespace; quote numeric-looking tags as shown above.

## Discover dependencies with a plugin

```yaml
include:
  - ref: registry.example.com/charts/web:1.4
    plugin:
      ref: registry.example.com/plugins/helm:1
      input:
        schemaVersion: 1
        release: web
        namespace: production
      blobs:
        chart:
          mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
```

`plugin.ref` selects a plugin from your registry by tag or digest. These example coordinates are placeholders; publish the plugin before using them. Creation runs it automatically, then captures its graph, generated declaration and discovered dependencies without attaching metadata to the source artifact.

`input` is a literal JSON object, defaults to `{}`, and accepts nested JSON values. File paths have no special meaning. Each `blobs` entry fills a top-level input field with verified, base64-encoded bytes from exactly one artifact layer matching `mediaType`. A bound field cannot also appear in `input`.

A plugin cannot be combined with `dependencies.follow: none` or `dependencies.select`. Locked transfers do not run plugins; latest pull requires `--allow-plugins`. See [dependency plugins](plugins.md) for publishing and runtime limits, and [Helm discovery](helm.md) for its input fields.

## Select referrers

```yaml
defaults:
  referrers:
    include: none
include:
  - ref: registry.example.com/apps/api:stable
    referrers:
      include: all
```

`defaults.referrers` applies to all inputs; per-input fields override inherited values. `include` accepts `all` or `none`. `require` is a list of `{digest: "sha256:..."}` entries using complete attachment manifest digests. Required referrers remain mandatory with `include: none` and must belong to the selected subject in its source repository. Setting `require` replaces the inherited list.

Selected artifact tags must not overlap reserved referrer inventory tags, including when using `tags: []`; see [registry behavior](registry.md).

## Control dependency following

```yaml
defaults:
  dependencies:
    follow: auto
include:
  - ref: registry.example.com/charts/web:1.4
    dependencies:
      select:
        context: production
  - ref: registry.example.com/components/standalone:v1
    dependencies:
      follow: none
```

`auto` follows a single matching [dependency declaration](dependencies.md). Multiple matches require `select` with exactly one `context` or complete declaration manifest `digest`. `defaults.dependencies` applies across inputs; input fields override it. Explicit `follow: none` clears any inherited selection and cannot be combined with a new selection.

Following declarations and including referrers are separate policies. A selected declaration is captured even with `referrers.include: none`; `dependencies.follow: none` stops declared dependency expansion while preserving mandatory OCI graph edges.

See [bundle lifecycle](concepts/bundles.md) for captured content and [locked and latest pulls](transfers.md#locked-and-latest-pulls) for refreshing selectors.
