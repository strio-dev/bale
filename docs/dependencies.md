# Dependency declarations

A dependency declaration is an OCI attachment listing the additional artifacts its subject needs. Bale follows it when creating a bundle. The declaration carries references; the bundle locks their resolved digests.

## Author and attach a declaration

Write `dependencies.yaml`:

```yaml
context: production
dependencies:
  - ref: registry.example.com/images/web:stable
  - ref: registry.example.com/components/worker:v2
    select:
      context: production
  - ref: other.example.com/images/database:17
    follow: none
```

| Field | Meaning |
| --- | --- |
| `context` | Optional declaration label, such as `production`. |
| `dependencies` | Required list; use `[]` for an empty declaration. No dependency IDs are needed. |
| `ref` | Required explicit registry reference with a tag or digest. |
| `follow` | `auto` by default; `none` includes this dependency without expanding its declarations. |
| `select` | Choose the dependency's own declaration by exactly one `context` or complete manifest `digest`. Incompatible with `follow: none`. |

YAML authoring omits `schemaVersion` and contains exactly one document. Unknown or duplicate fields, aliases, anchors, merge keys and nonstring scalars are rejected. String fields must be nonempty without surrounding whitespace. Equivalent entries are deduplicated; shared content is stored by digest.

```sh
bale dependencies attach registry.example.com/charts/web:1.4 -f dependencies.yaml
bale dependencies list registry.example.com/charts/web:1.4
bale dependencies inspect registry.example.com/charts/web:1.4
```

Use registry coordinates you control and have permission to write. Attach pins the subject once and publishes normalized JSON metadata. It does not resolve dependency tags, contact their registries or upload their content.

| Command | Result |
| --- | --- |
| `attach` | Attachment manifest digest; JSON output includes confirmed `published` and `discoverable` states. |
| `list` | Verified declaration descriptors, without parsing payloads. Use `.Digest` to choose a declaration in a recipe. |
| `inspect` | Validated declarations with `repository`, `subject`, `descriptor` and `declaration`, sorted by digest. It does not select or follow dependencies. |

List and inspect default to JSON arrays (`[]` when empty). For authentication, HTTP scopes and output templates, see [CLI conventions](reference/cli.md).

## Generate with a plugin

A [recipe plugin](recipes.md#discover-dependencies-with-a-plugin) generates a declaration during creation without writing a source attachment. For standalone generation or attachment with `--plugin` or `--plugin-file`, follow [manual plugin authoring](plugins.md#generate-or-attach-a-declaration-manually). These commands validate the output and leave dependency references unresolved until capture.

## Payload and envelope

Registry payloads use JSON with an explicit version:

```json
{
  "schemaVersion": 1,
  "dependencies": [
    {"ref": "registry.example.com/images/web:stable"}
  ]
}
```

The artifact type is `application/vnd.bale.dependencies.v1+json`. Its OCI manifest has the pinned artifact as its `subject`, an empty JSON config and one JSON layer with the same artifact type. JSON parsing rejects unknown fields, duplicate keys, nulls and trailing values.

This attachment is not a bundle: publishing it stores metadata only. The [SDK](sdk.md#author-dependency-declarations) exposes the same authoring and attachment operations.

## Following and selecting declarations

Creation automatically follows a single matching declaration, recursively locking its dependencies, complete multiarchitecture graphs and selected referrers. Tags are resolved at creation, and the bundle records both original selectors and pinned digests. More than one matching declaration requires an explicit `dependencies.select` in the [recipe](recipes.md#control-dependency-following).

The input's selection applies to that artifact. Each declaration entry can select its dependency's declaration independently. `follow: none` stops declaration expansion but retains mandatory OCI graph edges. Referrer inclusion remains separate: a selected declaration is retained even with `referrers.include: none`.

## Captured resolutions and limits

Locked pull uses captured digests. Latest pull resolves the recipe and dependency tags again, with `--allow-plugins` required for plugin execution. Nested Bale bundles retain their existing captured graphs in either mode. See [bundle lifecycle](concepts/bundles.md) and [inspection](inspection.md).

Declaration payloads, their manifests and candidate referrer envelopes are limited to 4 MiB each. Automatic following permits at most 10,000 dependency states; exceeding a limit fails capture.

Bale treats application payloads as opaque. Discovery integrations prepare these generic declarations; consumer integrations use [destination records](destinations.md) to map the captured direct dependencies to their delivered locations.
