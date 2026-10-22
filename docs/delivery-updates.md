# Delivery updates

Carry edited publication paths onto a refreshed bundle. Keep both snapshots: the old delivery YAML names captured artifacts, but only the old bundle records which selectors chose them.

## Update a file

With [locked and refreshed layouts](transfers.md#locked-and-latest-pulls):

```sh
bale delivery update --layout ./locked ./refreshed \
  -f bale.delivery.yaml -o refreshed.delivery.yaml
bale push ./refreshed mirror.example.com/releases/v1:bundle \
  --delivery refreshed.delivery.yaml --dry-run
```

Registry references work too:

```sh
bale delivery update registry.example.com/bundles/release:v1 \
  registry.example.com/bundles/release:v2 \
  -f old.delivery.yaml -o new.delivery.yaml
```

`--file/-f` selects YAML bound to the old bundle; `--output/-o` requires a new file. Both inputs are registry references unless `--layout` is set.

## Matching paths

The new [delivery file](transfers.md#export-delivery-paths) contains every refreshed artifact and binds to the new digest:

- An exact repository/digest match retains its explicit path.
- An input, dependency or plugin selector can carry a path to a changed digest when both resolution sets contain one artifact.
- Repeated aliases deduplicate; several old artifacts can converge when their paths agree.
- Unclaimed artifacts receive defaults, and removed artifacts are reported.

Every old row is an explicit claim, even if its path equals the default. Omitted rows make no claim. `push --map` settings are not carried forward; omit a new row if that artifact should use repository-wide mapping instead.

Nested inventories participate in matching. Selector evidence is aggregated by role and full reference, never by input position; differing nested captures can make a selector ambiguous. Changed platform children, declarations and arbitrary referrers without selector evidence receive defaults rather than guessed paths.

## Resolve conflicts

Bale writes no file if paths split across targets, differing paths converge, or selectors have multiple possible resolutions. An exact artifact that survives while its tag moves elsewhere can also have competing claims.

The report shows the disputed assignments and selector evidence. Copy the old delivery file, remove disputed rows from the copy, then rerun the update. Edit the resulting defaults and review them with `push --dry-run`.

## Reports and validation

The readable report labels paths `preserved`, `moved`, `defaulted` or `removed`. For automation:

```sh
bale delivery update --layout ./locked ./refreshed \
  -f bale.delivery.yaml -o refreshed.report.delivery.yaml --format json
```

JSON includes `before`, `after`, `changes`, `conflicts` and, on success, a candidate `delivery`. Conflicts return a report with status 125 and no output file; invalid inputs or read/template failures return 125 without a report.

Update reads manifests and interpreted metadata, including nested bundles and selected declarations. It does not refresh selectors, run plugins or check opaque payloads. Registry references resolve once; local updates stay offline and leave both layouts unchanged. [Push preview](transfers.md#push-to-a-destination) validates the full local graph before publication.
