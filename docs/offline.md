# Offline packaging

Create a complete local bundle using read access to the source registries, carry it as one tar file, and choose the receiving registry when you push. No writable holding registry or container runtime is required.

The [Helm offline example](examples/helm-offline/README.md) applies this workflow to a chart, its automatically discovered images and the native Helm integration, through deployment from the receiving registry.

## Capture from readable sources

Write a destination-free `bale.yaml`:

```yaml
include:
  - registry.example.com/apps/api:stable
  - registry.example.com/apps/worker:1.4
```

Capture directly into a new OCI layout and export delivery paths from that exact snapshot:

```sh
bale create --layout -f bale.yaml ./release \
  --output-delivery bale.delivery.yaml
bale inspect --layout ./release
```

Creation follows selected dependency declarations and runs configured [discovery plugins](plugins.md). It captures their OCI graphs, generated declarations, multiarchitecture images, nested Bale bundles and selected referrers. Original artifact bytes and digests remain unchanged. It reads source registries without publishing to them.

Use `--dry-run` to preview the captured inventory without retaining a layout. `--output-delivery` can also export the prospective paths during a dry run. Actual creation requires a new destination path; an existing file or directory is never replaced.

## Choose paths and save

Edit `path` values in `bale.delivery.yaml` for any artifact that needs a specific destination repository. Keep its `bundle` digest and artifact `ref` values intact. Paths are relative to the receiving bundle repository; omitted rows use defaults and remain included. See [delivery paths](transfers.md#export-delivery-paths).

```sh
bale push ./release mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml --dry-run
bale save ./release -o release.tar
```

Save validates the complete captured graph and writes an uncompressed tar archive containing the OCI layout. Shared content is stored once by digest, including content shared by nested bundles. This is the same OCI graph, with the same bundle digest. Save does not contact a registry, load credentials or execute plugins.

Carry `release.tar` and the edited `bale.delivery.yaml` across the disconnected boundary. The delivery file is separate so you can review and change placement without changing the bundle. Save refuses an existing archive path.

## Restore and deliver

On the receiving side, source registries and the original layout are no longer needed:

```sh
bale load -i release.tar ./restored
bale inspect --layout ./restored
bale inspect --layout ./restored --output-delivery default.delivery.yaml
bale push ./restored mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml --dry-run
bale push ./restored mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml
```

Load validates the archive and complete bundle graph before installing a new OCI layout. It performs no registry or plugin operations and refuses an existing destination. The restored directory works with inspection, comparison, delivery updates, preview and push just like a pulled layout. Commands take layout directories; restore an archive before using those commands.

The exported `default.delivery.yaml` contains regenerated defaults; keep your carried file for authored paths. Preview needs neither destination access nor credentials. Actual push needs permission to publish to the receiving registry and checks its existing tags. It preserves captured tags, selected referrers and artifact digests, then publishes [destination records](destinations.md) for consumers.

```sh
bale pull mirror.example.com/releases/v1:bundle ./verified
bale inspect --layout ./verified
```

This final locked pull reads the receiving registry alone. Registry-backed creation and [locked/latest pulls](transfers.md#locked-and-latest-pulls) remain available when a registry is useful as the bundle store.

Registry access uses Docker credentials, including `DOCKER_CONFIG`. For a local HTTP registry, add `--plain-http localhost:5000` with its exact host. Use command-specific `--help` for output formatting and options.
