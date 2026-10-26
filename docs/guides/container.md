# Run Bale in a container

Use the Bale image with Podman or Docker to capture and deliver bundles. The image includes the CLI and CA certificates for Linux AMD64 and ARM64.

## Check the CLI

```sh
podman run --rm ghcr.io/strio-dev/bale:latest --help
```

Replace `latest` with a release version to pin it: Git tag `v1.2.3` uses image tag `1.2.3`. Stable releases update `latest`; prereleases keep versioned tags. Replace `+` with `_` in image tags containing build metadata.

## Capture a bundle

Save the [first-bundle recipe](../tutorials/first-bundle.md#write-a-recipe) as `bale.yaml`. Mount the workspace and a persistent local store:

```sh
bale_data="${XDG_DATA_HOME:-$HOME/.local/share}/bale"
mkdir -p "$bale_data"
podman run --rm --userns=keep-id --user "$(id -u):$(id -g)" \
  -v "$PWD:/work:Z" -v "$bale_data:/data/bale:Z" -v bale-cache:/cache \
  -e XDG_DATA_HOME=/data \
  ghcr.io/strio-dev/bale:latest \
  create -f bale.yaml -t release:v1 --output-delivery bale.delivery.yaml
```

- `$PWD:/work:Z` holds recipes, delivery files and archive outputs. Outputs have your host ownership; `:Z` supplies an SELinux label.
- The data mount retains bundle IDs, local names and shared blobs between containers. It uses the same host store as the native CLI.
- `bale-cache:/cache` retains compiled discovery plugins. This mount is optional; keep it private to one effective user.

Creation locks metadata in the store; save or push reads missing content from the pinned sources. Bale needs no privileged mode, container socket or nested runtime. Plugins run inside Bale.

## Save, restore and deliver

Keep the workspace, store mount and data-home setting on each invocation:

```sh
podman run --rm --userns=keep-id --user "$(id -u):$(id -g)" \
  -v "$PWD:/work:Z" -v "$bale_data:/data/bale:Z" -e XDG_DATA_HOME=/data \
  ghcr.io/strio-dev/bale:latest save release:v1 -o release.tar
podman run --rm --userns=keep-id --user "$(id -u):$(id -g)" \
  -v "$PWD:/work:Z" -v "$bale_data:/data/bale:Z" -e XDG_DATA_HOME=/data \
  ghcr.io/strio-dev/bale:latest load -i release.tar -t restored:v1
podman run --rm --userns=keep-id --user "$(id -u):$(id -g)" \
  -v "$PWD:/work:Z" -v "$bale_data:/data/bale:Z" -e XDG_DATA_HOME=/data \
  ghcr.io/strio-dev/bale:latest \
  push restored:v1 mirror.example.com/release:bundle \
  --delivery bale.delivery.yaml --dry-run
```

Keep source access available until save finishes. Use new archive and delivery file paths. Replace the destination, configure credentials below, then remove `--dry-run` to publish. See [offline packaging](../offline.md) to move the archive between machines.

## Registry credentials

Create a Docker-compatible credential directory with your registry login tool, then mount it read-only:

```sh
podman run --rm --userns=keep-id --user "$(id -u):$(id -g)" \
  -v "$PWD:/work:Z" -v "$bale_data:/data/bale:Z" -e XDG_DATA_HOME=/data \
  -v "$HOME/.config/bale/registry-auth:/registry:ro,Z" -e DOCKER_CONFIG=/registry \
  ghcr.io/strio-dev/bale:latest \
  push restored:v1 mirror.example.com/release:bundle --delivery bale.delivery.yaml
```

Use the same credential mount for creation or save when the source registries are private. The example expects `$HOME/.config/bale/registry-auth/config.json`. The image has no credential helpers, so the file must contain usable credentials. Keep it outside bundles and Git. For a private CA, mount its PEM file read-only and set `SSL_CERT_FILE` to that path. See [registry access](../registry.md#authentication-and-transport).

## Docker

On Linux, replace `podman` with `docker` and omit `--userns=keep-id`. Keep `--user "$(id -u):$(id -g)"` for host-owned output. The image defaults to UID/GID `65532:65532` and supports arbitrary UIDs with writable temporary, workspace, store and cache mounts.

## Build locally

From a checkout with [Nix set up](../contributing.md):

```sh
just container-build
podman run --rm oci:result-container:nix --version
```

Nix builds the current platform's OCI layout without a container runtime. Its local tag is `nix`; the version label and CLI report the build version.
