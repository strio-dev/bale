# CLI

Use `bale COMMAND --help` for a command's complete flags and operands. Options can follow operands. Registry references require an explicit host and tag or digest.

## Commands

| Command | Purpose |
| --- | --- |
| `create` | Lock recipe metadata in the local store; optionally name it with `-t`. |
| `pull` / `push` | Download a bundle into the store or deliver a stored bundle. |
| `save` / `load` | Write or restore an offline OCI archive. |
| `inspect` / `diff` | Read inventory or compare two snapshots. |
| `delivery export` / `update` | Export publication paths or carry edits forward to a refreshed snapshot. |
| `dependencies generate` / `attach` | Produce or attach a dependency declaration. |
| `dependencies list` / `inspect` | Read declaration descriptors or payloads. |
| `destinations inspect` | Read delivered dependency mappings for an artifact. |
| `plugins push` | Publish a discovery plugin `.tgz` package as an OCI artifact. |
| `ls` / `info` | List retained local bundles or show store information. |
| `tag` / `rm` / `prune` | Manage local names and retained bundles, or reclaim unreferenced content. |
| `version` | Print the CLI version. |

Bundle operands default to local names or IDs. Exact local names take precedence over unambiguous short bundle IDs; ambiguous prefixes fail. `inspect`, `diff` and delivery commands require `--registry` for remote references. `create`, `pull` and `load` accept repeatable `--tag/-t` names and print the retained bundle digest. Creation and loading need no positional output path; pull takes only its registry reference.

The default store is `$XDG_DATA_HOME/bale`, or `~/.local/share/bale` when `XDG_DATA_HOME` is unset. Global `--store PATH` selects another store and may appear before or after the command. Shared blobs are stored once by digest. `rm` removes retained references; `prune` explicitly reclaims unreferenced content.

`-f` means `--file` for creation and authoring commands, and `--format` for inspection and comparison. Repeat `--plain-http` and `--map` when needed; duplicate single-value options are rejected.

`ls` shows 12-character bundle IDs; `--no-trunc` prints complete digests and `-q/--quiet` prints only IDs. JSON and templates retain full digests. `dependencies ls` is an alias for `dependencies list`.

Use `create -f -` or `dependencies attach -f -` to read YAML from stdin. Plugin requests also accept `--input -`, with the same 8 MiB limit as file input.

## Output and errors

| Operation | Default output |
| --- | --- |
| Transfers, load, save to a file, attachment and plugin publication | Result digest. |
| `save` without `-o FILE`, or with `-o -` | OCI archive bytes on stdout. |
| Bundle, dependency and destination inspection | JSON array. |
| `create --dry-run` / `push --dry-run` | Readable inventory / delivery plan. |
| `diff` / `delivery update` | Readable comparison / update report. |
| `dependencies generate` | Dependency declaration YAML. |
| `delivery export` | Delivery YAML on stdout, or a new file selected with `-o FILE`. |

Transfers write progress to stderr; `--quiet` suppresses it. Commands offering `--format` accept `json` or a Go template. Templates run once per item for inspection arrays and once against the result for other commands:

```sh
bale inspect release:v1 -f '{{.Descriptor.Digest}}'
bale inspect release:v1 --format json
```

`save` defaults to stdout and `load` to stdin; `-o/--output FILE` and `-i/--input FILE` select files, with `-` selecting the corresponding stream. Archive output to a terminal is refused. Saving to stdout emits only archive bytes; `--format` requires a named output file. `tag` and `rm` have no output-format option.

Template helpers are `json`, `join`, `split`, `lower` and `upper`. Missing keys are errors; output is buffered so a failed template does not print a partial result.

Success returns **0**; errors print `Error:` to stderr and return **125**. A successful `diff` returns 0 even when it finds changes.

On failed `create`, `pull` or `push`, explicit JSON output can retain a result describing confirmed partial work. Default digest and template output stay empty. Do not treat a partial JSON report as completion. Batch bundle inspection retains successful entries and returns 125 if any operand fails; if all fail, its JSON output is `[]`.

## Validation and previews

Default creation retains a source-dependent metadata capture in the managed store. `create --layout` downloads a complete local layout; `create --registry` stores the complete graph in the supplied registry repository. These options are mutually exclusive. With `--layout`, create/pull/load require an explicit output directory, and inspect/diff/delivery/push/save take standalone layout paths instead of local store names.

Local inspection, delivery export and push preview verify metadata captures without source access. Inspection reports and JSON push plans identify them with `metadataOnly: true`. Complete layouts also receive full payload validation. Diff, delivery update and registry delivery-file export read verified metadata without checking opaque payload availability. Destination inspection validates its attached metadata without following recorded locations.

`create --dry-run` resolves source content and runs configured discovery plugins without retaining a capture. With `--registry`, it also reads the holding registry to check tag conflicts. Local creation needs no writable registry. `push --dry-run` validates local content and computes paths without registry access; it does not check missing source payloads, destination credentials, availability or tag conflicts.

Push reads missing payloads from a metadata capture's pinned sources, reusing destination blobs or registry mounts when possible. Save hydrates missing payloads into the shared store and exports a complete offline archive. Neither operation refreshes tags or runs plugins. Save from a complete stored bundle and load remain offline.

## Credentials and transport

Registry operations select a Docker-compatible credential file with `--authfile FILE`, then `REGISTRY_AUTH_FILE`. When neither is set, Bale uses `DOCKER_CONFIG` or the usual Docker config directory. Bale uses system certificate trust; set `SSL_CERT_FILE` to supply a CA file. Permit HTTP only for a selected host with `--plain-http HOST`. Local-only management commands have no registry flags. See [registry configuration](../registry.md#authentication-and-transport).

Discovery execution has its own [runtime limits and cache settings](plugin-runtime.md). Locked pulls, archive operations and inspection do not execute plugins.

## Transfer behavior

### Delivery files

A delivery file binds to one bundle digest. Rows identify manifests and indexes by source repository and digest, including dependencies, plugins, attachments and nested inventories. Opaque configs and payload blobs have no editable rows.

Explicit paths override repository-wide `--map` settings; omitted rows use normal mapping. Unknown or duplicate identities, invalid paths and mismatched bundle digests fail before publication. Required graph/referrer relationships can create additional colocated copies.

`delivery export BUNDLE` writes YAML to stdout; `-o FILE` requires a new file and `-o -` selects stdout explicitly. Exports regenerate defaults. Authored paths are separate from bundle content.

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

Inspection and local delivery export validate manifests and interpreted metadata in source-dependent captures, and the complete graph in full layouts. Diff and registry export read metadata without checking opaque payloads. None refresh selectors or run plugins.

Each registry reference resolves once, so comparing a mutable tag with itself uses one pinned snapshot. Repeated selectors are compared as sets. Detailed selector/declaration changes describe root bundles; nested inventory membership is included. Compare nested bundles separately for their details. Artifact identity is repository plus digest; opaque blobs are excluded. A tag moving to already-captured content still produces a selector change.

### Update reports

JSON reports contain `before`, `after`, `changes`, `conflicts` and, on success, `delivery`. Conflicts return 125 and a report without writing a file. Invalid inputs and read/template failures return 125 without a report.

### Publication

Bale traverses OCI/Docker manifests and indexes referenced as artifact layers; configs and other layers stay opaque. Distribution 2.8.3 requires manifest-layer content registered through both manifest and blob endpoints; Bale preserves its digest while doing so.

Subject artifacts reserve the fallback tag in each receiving repository. Bale validates inventories and child subjects, preserves prior inventory manifests, and rejects conflicting aliases/completion tags, including aliases captured with `tags: []`.

Push publishes children before the completion tag. Existing tags with different digests and conflicting repository assignments fail. Updates across repositories are not atomic: failure can leave confirmed publications, and retrying reuses content and repairs incomplete publication. See [publication coordination](../sdk.md#publication-coordination) for concurrent clients.
