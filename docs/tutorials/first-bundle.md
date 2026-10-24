# Capture your first bundle

Capture BusyBox from Docker Hub, save it as an archive, then restore it offline. Choose a destination registry when you are ready to deliver.

## Before you start

[Install Bale](../getting-started.md). You need access to Docker Hub, disk space for the image and an empty working directory. Capture needs neither a container runtime nor a writable registry.

## Write a recipe

```sh
mkdir bale-first-bundle
cd bale-first-bundle
```

Save this as `bale.yaml`:

```yaml
include:
  - docker.io/library/busybox:1.37
```

Bale resolves the tag to a digest and includes its platform images.

## Capture and inspect

```sh
bale create --layout -f bale.yaml ./release \
  --output-delivery bale.delivery.yaml
bale inspect --layout ./release -f '{{.Descriptor.Digest}}'
```

Both commands print the same bundle digest. `release/` contains the captured content; `bale.delivery.yaml` lists its publication paths.

View the full JSON inventory with:

```sh
bale inspect --layout ./release
```

The [inspection guide](../inspection.md) explains the report and formatting options.

## Save and restore

```sh
bale save ./release -o release.tar
bale load -i release.tar ./restored
bale inspect --layout ./restored -f '{{.Descriptor.Digest}}'
bale diff --layout ./release ./restored
```

The restored digest matches the original, and diff reports `Bundle changed: false`. Save and load work offline and store shared content once by digest. Carry `release.tar` to another machine and restore it there the same way.

Layouts, archives and delivery files require new output paths. Use different names when repeating the tutorial.

## Preview delivery

```sh
bale push ./restored mirror.example.com/releases/first:bundle \
  --delivery bale.delivery.yaml --dry-run
```

The placeholder destination needs no credentials for this preview. Edit `path` values in the delivery file to choose different member repositories, then preview again.

## Publish when ready

Replace the destination with a writable registry and configure [credentials](../registry.md#authentication-and-transport):

```sh
bale push ./restored mirror.example.com/releases/first:bundle \
  --delivery bale.delivery.yaml
```

Bale publishes the content and tags, then the completion tag. Continue with [transfers](../transfers.md) for repository mappings and refreshes, or [offline packaging](../offline.md) for receiving-side steps.
