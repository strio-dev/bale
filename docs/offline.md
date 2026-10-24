# Work offline

Save a complete bundle as one tar file, carry it to another machine, then deliver it without access to the sources. Capture needs neither a holding registry nor a container runtime. For a first capture, follow [your first bundle](tutorials/first-bundle.md).

## Capture from readable sources

Lock the references and dependencies in your [recipe](recipes.md):

```sh
bale create -f bale.yaml -t release:v1 \
  --output-delivery bale.delivery.yaml
```

Creation reads manifests and discovery inputs, then retains the locked graph metadata in Bale's local store. Ordinary image payloads remain in their source registries until save. [Transfers](transfers.md) also covers immediate full capture with `--layout` and registry-backed bundles.

## Choose paths and save

Edit the delivery file's `path` values to choose receiving repositories. Keep its digest and artifact references intact.

```sh
bale save release:v1 -o release.tar
```

Save downloads missing content from the pinned sources into the shared store, validates the complete graph and writes an uncompressed OCI-layout tar archive. Keep source access and credentials available until it finishes. Shared content is stored once by digest; the bundle digest stays unchanged. Use a new archive path.

Carry both `release.tar` and your edited delivery file. Keeping placement separate lets you change it without rebuilding the archive.

## Restore and deliver

On the receiving machine:

```sh
bale load -i release.tar -t restored:v1
bale inspect restored:v1
bale push restored:v1 mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml --dry-run
```

Use new archive and delivery file paths. Load validates the archive and complete graph before retaining it in the local store. Load, local inspection and push preview work offline. Saving an already complete stored bundle is offline too.

Replace the destination and configure [registry credentials](registry.md#authentication-and-transport), then publish and verify:

```sh
bale push restored:v1 mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml
bale pull mirror.example.com/releases/v1:bundle -t verified:v1
```

The final locked pull uses only the receiving registry. If you lose the delivery file, [export defaults](inspection.md#export-delivery-yaml) from the imported bundle; the archive cannot recover your path edits.

For charts, follow the [Helm offline example](examples/helm-offline/README.md).
