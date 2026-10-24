# CLI conventions

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
