# Update delivery paths

Carry your path edits onto a refreshed bundle. Keep both snapshots: the old bundle records which source selectors chose each artifact.

## Update a file

With [locked and refreshed layouts](transfers.md#locked-and-latest-pulls):

```sh
bale delivery update --layout ./locked ./refreshed \
  -f bale.delivery.yaml -o refreshed.delivery.yaml
bale push ./refreshed mirror.example.com/releases/v1:bundle \
  --delivery refreshed.delivery.yaml --dry-run
```

You can also use registry references:

```sh
bale delivery update registry.example.com/bundles/release:v1 \
  registry.example.com/bundles/release:v2 \
  -f old.delivery.yaml -o new.delivery.yaml
```

`--file/-f` selects the old delivery YAML; `--output/-o` requires a new file. Use `--layout` for local inputs.

## Matching paths

The new file binds to the refreshed bundle and lists every artifact:

- Exact repository/digest matches retain their paths.
- Unambiguous input, dependency and plugin selectors can carry paths to changed digests.
- New or unmatched artifacts receive defaults; removed artifacts are reported.

Nested inventories participate too. Every old row is an explicit path choice. `push --map` settings are separate and are not carried forward. See [matching rules](reference/cli.md#matching-delivery-paths) for aliases and ambiguous selectors.

## Resolve conflicts

If old path choices disagree or a selector matches several artifacts, Bale reports conflicts and writes no file. Copy the old YAML, remove disputed rows from the copy, then rerun the update. Edit the resulting defaults and review them with `push --dry-run`.

## Reports and validation

The readable report labels paths `preserved`, `moved`, `defaulted` or `removed`. Request JSON for automation:

```sh
bale delivery update --layout ./locked ./refreshed \
  -f bale.delivery.yaml -o refreshed.report.delivery.yaml --format json
```

Update reads captured metadata and leaves both bundles unchanged. It does not refresh tags, run plugins or check payloads. [Preview the push](transfers.md#push-to-a-destination) to validate paths and local content. Metadata captures still need their pinned sources during delivery. [Report fields and exit status](reference/cli.md#update-reports) are listed in the reference.
