# Delivery updates

Carry edited publication paths onto a refreshed bundle without rebuilding the delivery file by hand. The old snapshot supplies the captured identities and selectors behind those edits; the new snapshot supplies the refreshed inventory.

## Update a file

With [locked and refreshed layouts](transfers.md#locked-and-latest-pulls):

```sh
bale delivery update --layout ./locked ./refreshed \
  -f bale.delivery.yaml -o refreshed.delivery.yaml
bale push ./refreshed mirror.example.com/releases/v1:bundle \
  --delivery refreshed.delivery.yaml --dry-run
```

Registry references are supported too:

```sh
bale delivery update registry.example.com/bundles/release:v1 \
  registry.example.com/bundles/release:v2 \
  --file old.delivery.yaml --output new.delivery.yaml
```

`--file/-f` must name delivery YAML bound to the old bundle. `--output/-o` is a new file; existing files, including the input, are never overwritten. Both operands are registry references by default or local OCI layouts with `--layout`. Keep the old captured snapshot available; the edited YAML alone cannot establish which tag previously selected an artifact.

## Matching paths

The output uses the existing [delivery YAML schema](transfers.md#export-delivery-paths) and binds it to the new bundle digest. Every refreshed artifact receives a row.

- Exact source repository and digest identities retain their explicit paths.
- A captured input, dependency or plugin selector can carry a path to a new digest when its old and new resolution sets each contain one artifact.
- Repeated aliases deduplicate. Multiple old artifacts may converge on one target when their explicit paths agree.
- Unclaimed artifacts receive default paths. Removed identities are reported; identities successfully moved to another digest appear as moves.

Every row in the old file is an explicit path claim, including rows whose paths equal the defaults. Omitted rows make no claim. Repository-wide `push --map` settings are separate and are not carried into the file. Because the updated file contains the full inventory, its rows take precedence over later `--map` settings; omit a row from the new file to leave that artifact under repository-wide mapping.

Nested bundle inventories participate in exact matching. Original selector sets are aggregated across all captured snapshots by role and full reference. Bale never pairs snapshots or inputs by position. Differing nested captures can therefore make a selector ambiguous. Platform children, declarations and arbitrary referrers have no guaranteed original selector; changed artifacts without selector evidence are removed/defaulted rather than inferred from their type or position.

## Resolve conflicts

The command returns status 125 and writes no delivery file when an explicit path would split across targets, different paths converge on one target, or a changed selector has multiple possible resolutions. An artifact that survives exactly while its tag moves elsewhere also has two target claims and produces a conflict. Exact preservation does not silently override a competing tag-based claim.

The report identifies the old assignments, possible targets and selector evidence. To resolve ambiguity, make a copy of the old file and remove the disputed rows, run the update using that copy, then explicitly edit the new default paths. Review the resulting paths with `push --dry-run` before publication.

## Reports and validation

The readable report labels changes as `preserved`, `moved`, `defaulted` or `removed`. Conflict targets have diagnostics instead of proposed path changes. Use JSON or a Go template for automation:

```sh
bale delivery update --layout ./locked ./refreshed \
  -f old.yaml -o new.yaml --format json
```

JSON contains `before`, `after`, `changes`, `conflicts` and a candidate `delivery` on success. Conflicts retain their report on stdout with status 125 and no candidate file. Invalid inputs, read failures and template errors return status 125 without a report. Successful updates return status 0.

Each registry operand resolves once; identical reference strings share one pinned read. Update validates OCI envelopes, interpreted Bale metadata and selected declaration JSON, including nested metadata, without refreshing sources, discovering attachments, executing plugins, publishing content, or reading or checking opaque configs and payload layers. Local updates make no network requests, read no registry credentials and leave layouts unchanged. Only the requested new output file is written.

Metadata validation does not prove opaque payload availability or integrity. [Push planning](transfers.md#push-to-a-destination) verifies the complete local content and destination layout before publication.
