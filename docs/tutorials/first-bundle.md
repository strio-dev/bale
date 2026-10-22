# Capture your first bundle

Capture BusyBox from Docker Hub, save it as a portable archive, and restore it without contacting the source registry. You can choose a destination registry later.

## Before you start

[Install Bale](../getting-started.md). You need access to Docker Hub for capture, disk space for the image, and an empty working directory. No container runtime or writable registry is needed.

## Write a recipe

Start a new directory:

```sh
mkdir bale-first-bundle
cd bale-first-bundle
```

Save this as `bale.yaml`:

```yaml
include:
  - docker.io/library/busybox:1.37
```

The recipe names what to capture, without a destination. Bale resolves the tag to a digest and includes its platform images.

## Capture and inspect

```sh
bale create --layout -f bale.yaml ./release \
  --output-delivery bale.delivery.yaml
bale inspect --layout ./release -f '{{.Descriptor.Digest}}'
```

Creation prints a bundle digest. Inspection prints the same digest from the completed layout. `release/` holds the captured OCI content; `bale.delivery.yaml` lists editable publication paths for that snapshot.

To see the full inventory:

```sh
bale inspect --layout ./release
```

The JSON report includes the recipe, captured roots and resolved content. The [inspection guide](../inspection.md) covers comparisons and formatting.

## Save and restore

```sh
bale save ./release -o release.tar
bale load -i release.tar ./restored
bale inspect --layout ./restored -f '{{.Descriptor.Digest}}'
bale diff --layout ./release ./restored
```

The restored digest matches the original. Diff reports `Bundle changed: false`. Save and load need no registry access; shared content is stored once by digest. Carry `release.tar` across an offline boundary and restore it there in the same way.

Layouts, archives and exported delivery files require new paths. Keep the original layout for comparison, or use different names when repeating this tutorial.

## Preview delivery

You can preview publication without contacting a destination:

```sh
bale push ./restored mirror.example.com/releases/first:bundle \
  --delivery bale.delivery.yaml --dry-run
```

`mirror.example.com` is a placeholder. The preview shows where the captured artifacts would go; it needs no registry credentials. Edit `path` values in the delivery file if you want different member repositories, then preview again.

## Publish when ready

Actual delivery requires a writable registry. Replace the example reference below with your own registry and repository, and configure [registry credentials](../registry.md#authentication-and-transport):

```sh
bale push ./restored mirror.example.com/releases/first:bundle \
  --delivery bale.delivery.yaml
```

Bale publishes the captured content and tags, then the bundle's completion tag. Read [transfers](../transfers.md) for repository mappings and refreshing inputs, or [offline packaging](../offline.md) for archive and receiving-side details.
