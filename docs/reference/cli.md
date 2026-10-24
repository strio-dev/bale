# CLI

Use `bale COMMAND --help` for a command's complete flags and operands. Options can follow operands. Registry references require an explicit host and tag or digest.

## Commands

| Command | Purpose |
| --- | --- |
| `create` | Capture recipe inputs in a registry or, with `--layout`, a local directory. |
| `pull` / `push` | Retrieve a captured bundle or deliver a local layout. |
| `save` / `load` | Write or restore an offline OCI archive. |
| `inspect` / `diff` | Read inventory or compare two snapshots. |
| `delivery update` | Carry edited paths forward to a refreshed snapshot. |
| `dependencies generate` / `attach` | Produce or attach a dependency declaration. |
| `dependencies list` / `inspect` | Read declaration descriptors or payloads. |
| `destinations inspect` | Read delivered dependency mappings for an artifact. |
| `plugins push` | Publish a discovery plugin `.tgz` package as an OCI artifact. |
| `version` | Print the CLI version. |

`-f` means `--file` for creation and authoring commands, and `--format` for inspection and comparison. Repeat `--plain-http` and `--map` when needed; duplicate single-value options are rejected.

## Output and errors

| Operation | Default output |
| --- | --- |
| Transfers, archive operations, attachment and plugin publication | Result digest. |
| Bundle, dependency and destination inspection | JSON array. |
| `create --dry-run` / `push --dry-run` | Readable inventory / delivery plan. |
| `diff` / `delivery update` | Readable comparison / update report. |
| `dependencies generate` | Dependency declaration YAML. |

Transfers write progress to stderr; `--quiet` suppresses it. Commands offering `--format` accept `json` or a Go template. Templates run once per item for inspection arrays and once against the result for other commands:

```sh
bale inspect --layout ./release -f '{{.Descriptor.Digest}}'
bale inspect --layout ./release --format json
```

Template helpers are `json`, `join`, `split`, `lower` and `upper`. Missing keys are errors; output is buffered so a failed template does not print a partial result.

Success returns **0**; errors print `Error:` to stderr and return **125**. A successful `diff` returns 0 even when it finds changes.

On failed `create`, `pull` or `push`, explicit JSON output can retain a result describing confirmed partial work. Default digest and template output stay empty. Do not treat a partial JSON report as completion. Batch bundle inspection retains successful entries and returns 125 if any operand fails; if all fail, its JSON output is `[]`.

## Validation and previews

Full bundle inspection, local delivery-file export and push validate complete captured content. Diff, delivery update and registry delivery-file export read verified metadata without checking opaque payload availability. Destination inspection validates its attached metadata without following recorded locations.

`create --dry-run` contacts source registries and runs configured discovery plugins to resolve the inventory. It does not publish a bundle. `push --dry-run` validates the local graph and computes paths without registry access; it does not check destination credentials, availability or tag conflicts.

## Credentials and transport

Bale reads Docker credentials from `DOCKER_CONFIG`, or the usual Docker config directory, and uses system certificate trust. Set `SSL_CERT_FILE` to supply a CA file where needed. Permit HTTP only for a selected host with `--plain-http HOST`. See [registry configuration](../registry.md#authentication-and-transport).

Discovery execution has its own [runtime limits and cache settings](plugin-runtime.md). Locked pulls, archive operations and inspection do not execute plugins.

## Transfer behavior

### Delivery files

A delivery file binds to one bundle digest. Rows identify manifests and indexes by source repository and digest, including dependencies, plugins, attachments and nested inventories. Opaque configs and payload blobs have no editable rows.

Explicit paths override repository-wide `--map` settings; omitted rows use normal mapping. Unknown or duplicate identities, invalid paths and mismatched bundle digests fail before publication. Required graph/referrer relationships can create additional colocated copies.

Exports require a new file and regenerate defaults. Authored paths are separate from bundle content.

### Matching delivery paths

Update matches both snapshots, including nested inventories:

- Exact repository/digest matches retain explicit paths.
- Input, dependency and plugin selectors carry paths to changed digests only when both resolution sets identify one artifact.
- Repeated aliases deduplicate. Several old artifacts can converge if their paths agree.
- Unclaimed artifacts receive defaults; removed artifacts are reported.

Old rows are explicit claims, including default-looking paths; omitted rows make no claim. Update does not carry forward `--map` settings.

Selectors are grouped by role and full reference, not input position. Different nested captures can make one ambiguous. Platform children, declarations and arbitrary referrers without selector evidence receive defaults.

Conflicting paths or multiple selector resolutions prevent output. An artifact that remains while its tag moves elsewhere can also receive competing claims.

### Inspection and comparison

Inspection and local delivery export validate the complete graph. Diff and registry export read manifests and interpreted metadata without checking opaque payloads. None refresh selectors or run plugins.

Each registry reference resolves once, so comparing a mutable tag with itself uses one pinned snapshot. Repeated selectors are compared as sets. Detailed selector/declaration changes describe root bundles; nested inventory membership is included. Compare nested bundles separately for their details. Artifact identity is repository plus digest; opaque blobs are excluded. A tag moving to already-captured content still produces a selector change.

### Update reports

JSON reports contain `before`, `after`, `changes`, `conflicts` and, on success, `delivery`. Conflicts return 125 and a report without writing a file. Invalid inputs and read/template failures return 125 without a report.

### Publication

Bale traverses OCI/Docker manifests and indexes referenced as artifact layers; configs and other layers stay opaque. Distribution 2.8.3 requires manifest-layer content registered through both manifest and blob endpoints; Bale preserves its digest while doing so.

Subject artifacts reserve the fallback tag in each receiving repository. Bale validates inventories and child subjects, preserves prior inventory manifests, and rejects conflicting aliases/completion tags, including aliases captured with `tags: []`.

Push publishes children before the completion tag. Existing tags with different digests and conflicting repository assignments fail. Updates across repositories are not atomic: failure can leave confirmed publications, and retrying reuses content and repairs incomplete publication. See [publication coordination](../sdk.md#publication-coordination) for concurrent clients.
