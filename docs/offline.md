# Offline packaging

Carry a complete bundle as one tar file, then deliver it without access to the original sources. No holding registry or container runtime is required. For a step-by-step capture, follow [your first bundle](tutorials/first-bundle.md).

## Capture from readable sources

Create a local layout from your [recipe](recipes.md), with delivery paths from the same capture:

```sh
bale create --layout -f bale.yaml ./release \
  --output-delivery bale.delivery.yaml
```

Capture reads sources without publishing to them. The layout includes selected dependencies, plugin content, nested bundles, platform images and referrers. See [transfers](transfers.md) for capture previews and registry-backed bundles.

## Choose paths and save

Edit `path` values in `bale.delivery.yaml` if artifacts need particular receiving repositories. Keep its bundle digest and artifact references intact; see [delivery paths](transfers.md#export-delivery-paths).

```sh
bale save ./release -o release.tar
```

Save validates the complete graph and writes an uncompressed OCI-layout tar archive. Content shared across artifacts or nested bundles is stored once by digest. The bundle digest stays unchanged.

Carry `release.tar` and your edited delivery file. Delivery paths remain separate so placement can change without rebuilding the archive. Save requires a new archive path.

## Restore and deliver

```sh
bale load -i release.tar ./restored
bale inspect --layout ./restored
bale push ./restored mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml --dry-run
```

Load validates the archive and complete graph before installing a new layout. Save, load and local preview use no registry credentials or plugins. Restore an archive before using layout commands such as inspect, diff or push.

Replace `mirror.example.com/releases/v1:bundle` with a writable destination and configure [registry access](registry.md#authentication-and-transport), then publish:

```sh
bale push ./restored mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml
bale pull mirror.example.com/releases/v1:bundle ./verified
```

The final locked pull reads the receiving registry alone. The original layout and source registries are no longer needed.

If you lose the delivery file, [export defaults](inspection.md#export-delivery-yaml) from the restored bundle; authored edits cannot be recovered from the archive. See the [Helm offline example](examples/helm-offline/README.md) to carry a chart, its discovered images and the native Helm integration through deployment.
