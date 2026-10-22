# Recipes

Recipes are authored in YAML; generated bundle metadata is JSON. Every source requires an explicit registry and tag or digest. Recipes select captured content; generated [delivery YAML](transfers.md#export-delivery-paths) controls where that content is pushed. Changing delivery paths does not require recapturing the recipe.

## Select references and tags

```yaml
include:
  - registry.example.com/apps/api:stable
  - repository: registry.example.com/apps/worker
    tags: ["1.4", stable]
  - repository: registry.example.com/shared/tools
    tags: []
```

A repository input requires `tags`. A nonempty list selects literal tags; `[]` captures all observed tags. `[all]` selects the literal tag named `all`. Creation pins the observed digests and preserves complete multiarchitecture graphs.

## Discover dependencies with a plugin

Select an OCI discovery plugin on an input:

```yaml
include:
  - ref: registry.example.com/charts/web:1.4
    plugin:
      ref: registry.example.com/plugins/helm:1
      input:
        schemaVersion: 1
        release: web
        namespace: production
        values: {replicaCount: 2}
      blobs:
        chart:
          mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
```

Creation resolves and runs the plugin automatically, then captures its graph and generated declaration with the discovered dependencies. It does not attach the declaration to the source artifact. `input` is a literal JSON object, defaults to `{}`, and may contain nested nulls, numbers and booleans. File paths have no special meaning.

Each `blobs` entry binds one top-level input field to exactly one artifact layer with the requested media type. Bale verifies and base64-encodes those bytes for the request. Bound fields cannot also appear in `input`. Do not combine a plugin with `dependencies.follow: none` or `dependencies.select`.

Locked transfers do not execute plugins. Latest pull requires `--allow-plugins` to refresh a plugin recipe. See [dependency plugins](plugins.md) for publication, manual runs and runtime policy, or [Helm discovery](helm.md) for render-profile fields.

## Select referrers

Referrers default to `all`. An input can set `referrers.include: none` or require exact referrer digests with `referrers.require: [{digest: "sha256:..."}]`. Requirements remain mandatory with `none` and must match the selected subject in its source repository. Replace the abbreviated digest with a complete digest.

```yaml
include:
  - ref: registry.example.com/apps/api:stable
    referrers:
      include: none
```

Captured aliases must not overlap the standard referrer inventory tags reserved by Bale. This also applies to `tags: []`; see [registry behavior](registry.md).

## Control dependency following

Bale automatically follows one matching generic dependency declaration. Use `follow: none` to stop semantic expansion, or select a declaration when several match:

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

Use `select: {digest: "sha256:..."}` with a complete digest instead of context to select an exact declaration manifest. Disabling following preserves mandatory OCI graph edges. Optional referrers are controlled separately: a declaration selected for dependency following is still captured with `referrers.include: none`.

See [dependency declarations](dependencies.md) for attachment authoring and recursive selection. [Locked and latest pulls](transfers.md#locked-and-latest-pulls) control whether a later pull uses captured resolutions or refreshes the recipe.
