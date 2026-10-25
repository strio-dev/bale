# Capture your first bundle

Capture BusyBox from Docker Hub, save it as an archive, then restore it offline. Choose a destination registry when you are ready to deliver.

## Before you start

[Install Bale](../getting-started.md). You need access to Docker Hub and disk space for the image. The next step creates a working directory. Capture needs neither a container runtime nor a writable registry.

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

Bale resolves the tag and its platform images to digests without downloading their image layers.

## Capture and inspect

```sh
bale create -f bale.yaml -t release:v1 \
  --output-delivery bale.delivery.yaml
bale ls
bale inspect release:v1 -f '{{.Descriptor.Digest}}'
```

Create and inspect print the same bundle digest. `release:v1` names it in Bale's local store; `bale.delivery.yaml` lists publication paths. Keep access to Docker Hub until saving or pushing the bundle.

View the full JSON inventory with:

```sh
bale inspect release:v1
```

The report shows `metadataOnly: true` while payloads are missing from the store. The [inspection guide](../inspection.md) explains its validation and formatting options.

## Save and restore

```sh
bale save release:v1 -o release.tar
bale load -i release.tar -t restored:v1
bale inspect restored:v1 -f '{{.Descriptor.Digest}}'
bale diff release:v1 restored:v1
```

Save downloads the pinned payloads into the shared local store and writes a complete archive. Load works offline and adds the `restored:v1` name. The restored digest matches the original, and diff reports `Bundle changed: false`. Shared content is stored once by digest. Carry `release.tar` to another machine and restore it there the same way.

The archive and delivery output files must not already exist. Bundle names refer to retained IDs in the local store; no bundle output directory is needed.

## Preview delivery

```sh
bale push restored:v1 mirror.example.com/releases/first:bundle \
  --delivery bale.delivery.yaml --dry-run
```

The placeholder destination needs no credentials for this preview. Edit `path` values in the delivery file to choose different member repositories, then preview again.

## Publish when ready

Replace the destination with a writable registry and configure [credentials](../registry.md#authentication-and-transport):

```sh
bale push restored:v1 mirror.example.com/releases/first:bundle \
  --delivery bale.delivery.yaml
```

Bale publishes the content and tags, then the completion tag. Continue with [transfers](../transfers.md) for repository mappings and refreshes, or [offline packaging](../offline.md) for receiving-side steps.
