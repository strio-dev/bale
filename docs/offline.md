# Work offline

Save a complete bundle as one tar file, carry it to another machine, then deliver it without access to the sources. Capture needs neither a holding registry nor a container runtime. For a first capture, follow [your first bundle](tutorials/first-bundle.md).

## Capture from readable sources

Create a layout from your [recipe](recipes.md):

```sh
bale create --layout -f bale.yaml ./release \
  --output-delivery bale.delivery.yaml
```

Bale reads the sources and captures selected dependencies, plugins, nested bundles, platform images and referrers. [Transfers](transfers.md) also covers previews and registry-backed bundles.

## Choose paths and save

Edit the delivery file's `path` values to choose receiving repositories. Keep its digest and artifact references intact.

```sh
bale save ./release -o release.tar
```

Save validates the graph and writes an uncompressed OCI-layout tar archive. Shared content is stored once by digest; the bundle digest stays unchanged. Use a new archive path.

Carry both `release.tar` and your edited delivery file. Keeping placement separate lets you change it without rebuilding the archive.

## Restore and deliver

On the receiving machine:

```sh
bale load -i release.tar ./restored
bale inspect --layout ./restored
bale push ./restored mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml --dry-run
```

Use paths that do not already exist for the archive, restored layout and exported delivery file. Load validates the archive and complete graph before publishing the new layout. Save, load, local inspection and push preview work offline.

Replace the destination and configure [registry credentials](registry.md#authentication-and-transport), then publish and verify:

```sh
bale push ./restored mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml
bale pull mirror.example.com/releases/v1:bundle ./verified
```

The final locked pull uses only the receiving registry. If you lose the delivery file, [export defaults](inspection.md#export-delivery-yaml) from the restored bundle; the archive cannot recover your path edits.

For charts, follow the [Helm offline example](examples/helm-offline/README.md).
