# Dependency declarations

A generic dependency declaration tells Bale which additional OCI references to include when creating a bundle. It is an OCI referrer of the artifact that needs those dependencies.

## Author and attach a declaration

Write `dependencies.yaml` with explicit registry references. The YAML authoring format is strict: unknown or duplicate fields are rejected. The YAML schema version is implicit; omit `schemaVersion`. The published registry JSON carries `schemaVersion: 1`. `dependencies` is required (use `[]` for an empty declaration).

```yaml
context: production
dependencies:
  - ref: registry.example.com/images/web:stable
  - ref: registry.example.com/components/worker:v2
    select:
      context: production
    follow: auto
  - ref: other.example.com/images/database:17
    follow: none
```

An entry's `select` may contain a declaration `digest` instead of `context`; the two are mutually exclusive. `follow` defaults to `auto`, and may be `none` to stop expanding that dependency's declarations.

```sh
bale dependencies attach registry.example.com/charts/web:1.4 -f dependencies.yaml
bale dependencies list registry.example.com/charts/web:1.4
bale dependencies list registry.example.com/charts/web:1.4 -f '{{.Digest}}'
bale dependencies inspect registry.example.com/charts/web:1.4
bale dependencies inspect registry.example.com/charts/web:1.4 -f '{{.Declaration.Context}} {{json .Declaration.Dependencies}}'
```

Attach validates the complete YAML before registry access, resolves the subject once, and attaches to that pinned digest. Dependency tags and declaration selectors remain unresolved: attach never contacts their registries. The published payload is normalized JSON in the envelope described below. Existing Docker credentials and repeatable `--plain-http HOST` registry scopes apply.

Attach prints the attachment manifest digest by default. `--format json` prints the full SDK `AttachResult`, including `published` and `discoverable`; Go templates receive that result (for example `--format '{{.Descriptor.Digest}}'`). `-q`/`--quiet` suppresses progress on stderr.

List returns only verified referrer descriptors with Bale's dependency artifact type. Its default output is an indented JSON array (`[]` when empty). `-f`/`--format` accepts JSON or a Go template executed once per OCI descriptor, with fields such as `.Digest`, `.MediaType`, `.ArtifactType`, and `.Annotations`. It lists declaration envelopes without parsing their JSON payloads. Use a returned manifest digest as `dependencies.select.digest` in a recipe when multiple declarations exist.

Inspect reads and validates every attached dependency declaration payload, including its `context` and dependency references. It returns an indented JSON array of records with `repository`, `subject`, `descriptor`, and `declaration` fields (`[]` when empty), sorted by declaration digest. `-f`/`--format` runs a Go template per record: `.Declaration.Context` prints the context, `.Declaration.Dependencies` exposes the entries, and `.Descriptor.Digest` identifies the declaration for recipe selection. Inspect neither resolves dependency references nor chooses a declaration when multiple are attached. Invalid declarations fail inspection without partial output.

Attach publishes a declaration, list discovers its descriptor, and inspect reads its validated payload. All three use the existing registry authentication and exact-host HTTP scopes.

After attaching, use the normal `bale create -f bale.yaml registry.example.com/bundles/release:v1` workflow. Capture automatically follows a single matching declaration; attaching multiple declarations requires an explicit selection as described below. Bale captures OCI dependencies and does not rewrite consumer configuration.

## Generate with a plugin

Configure an OCI plugin in the [recipe](recipes.md#discover-dependencies-with-a-plugin) to discover and capture dependencies automatically during `create`. Bale retains the generated declaration in the bundle without writing a source attachment.

To review or publish a declaration separately, run a registry plugin with application-specific JSON input:

```sh
bale dependencies generate --plugin registry.example.com/plugins/references:1 --input request.json > dependencies.yaml
```

The command validates the declaration JSON and writes authoring YAML for review and attachment. Attach can run the plugin directly with `--plugin registry.example.com/plugins/references:1 --input request.json`, instead of `-f dependencies.yaml`. Use `--plugin-file references.wasm` for local development. Dependency references remain unresolved in these manual workflows.

See [dependency plugins](plugins.md) for OCI publication, the module contract, cache settings and resource limits. For packaged charts, the [Helm discovery plugin](helm.md) renders one profile offline and declares images from supported workloads, hooks and subcharts.

## Payload and envelope

```json
{
  "schemaVersion": 1,
  "dependencies": [
    {"ref": "registry.example.com/images/web:stable"},
    {"ref": "other.example.com/images/database:17"}
  ]
}
```

The declaration uses artifact type `application/vnd.bale.dependencies.v1+json`, an OCI empty JSON config, and one JSON layer of that same artifact type. Its `subject` is the pinned artifact descriptor. Declaration JSON is strict; references require an explicit registry and tag or digest. There are no dependency IDs, and repeated dependencies share captured content by digest.

Publishing a declaration uploads metadata only. It does not resolve or upload the referenced dependencies, and the declaration itself is not a bundle. Use the CLI above or the [SDK authoring and attachment APIs](sdk.md#author-dependency-declarations) to prepare and publish it.

## Following and selecting declarations

Bundle creation automatically follows a single matching declaration and recursively captures its dependencies, complete multiarchitecture graphs and selected referrers. The bundle inventory records resolved digests, including when no declaration was found. Multiple matching declarations require an explicit selection.

An optional declaration `context` distinguishes choices such as production and development. An input's `dependencies.select` chooses a context or exact declaration manifest digest. See [recipe policies](recipes.md#control-dependency-following) for an example.

Selection applies to the input artifact. Declaration entries select their own declarations through an optional `select` object, using `context` or `digest`. An entry may set `follow: none` to include that dependency without expanding its declarations. Disabling following stops semantic expansion while preserving mandatory OCI graph edges.

`referrers.include` separately controls optional attachments. A declaration selected for dependency following remains captured with `referrers.include: none`.

## Captured resolutions and limits

[Locked pull](transfers.md#locked-and-latest-pulls) uses captured digests without discovering new declarations or resolving dependency tags. Latest pull resolves the recipe and its dependency tags again; recipes with plugins require `--allow-plugins` before their discovery code runs. An included nested Bale bundle keeps its existing captured graph in either mode. [Inspection](inspection.md) exposes the captured resolutions and selected declaration JSON.

Declaration payloads and their OCI manifests are limited to 4 MiB each. Automatic following also limits candidate referrer envelopes to 4 MiB and traversal to 10,000 dependency states. Exceeding either limit fails capture.

Bale core treats application payloads as opaque. The [Helm plugin](helm.md) prepares generic declarations from an offline render. Helm/Flux consumption and reference rewriting remain integration work.

After bundle push, consumers can read [destination records](destinations.md) attached to the delivered artifact. They report captured direct dependencies and confirmed delivery coordinates; they are separate from producer declarations and do not affect declaration selection.
