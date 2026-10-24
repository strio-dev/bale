# Run Bale in a container

Use the Bale image with Podman or Docker to capture and deliver bundles. The image includes the CLI and CA certificates for Linux AMD64 and ARM64.

## Check the CLI

```sh
podman run --rm ghcr.io/strio-dev/bale:latest --help
```

Replace `latest` with a published release tag to pin a version. Stable releases update `latest`; prereleases keep versioned tags. Replace `+` with `_` in image tags containing build metadata.

## Capture a bundle

Save the [first-bundle recipe](../tutorials/first-bundle.md#write-a-recipe) as `bale.yaml`, then mount its working directory:

```sh
podman run --rm --userns=keep-id --user "$(id -u):$(id -g)" \
  -v "$PWD:/work:Z" -v bale-cache:/cache \
  ghcr.io/strio-dev/bale:latest \
  create --layout -f bale.yaml ./release --output-delivery bale.delivery.yaml
```

The mounts have two purposes:

- `$PWD:/work:Z` makes the project writable inside the container. Outputs have your host ownership; `:Z` supplies an SELinux label.
- `bale-cache:/cache` retains compiled discovery plugins between runs. This mount is optional; keep the cache private to one effective user.

Bale needs no privileged mode, container socket or nested runtime. Plugins run inside Bale.

## Save, restore and deliver

Use the same workspace mount:

```sh
podman run --rm --userns=keep-id --user "$(id -u):$(id -g)" -v "$PWD:/work:Z" \
  ghcr.io/strio-dev/bale:latest save ./release -o release.tar
podman run --rm --userns=keep-id --user "$(id -u):$(id -g)" -v "$PWD:/work:Z" \
  ghcr.io/strio-dev/bale:latest load -i release.tar ./restored
podman run --rm --userns=keep-id --user "$(id -u):$(id -g)" -v "$PWD:/work:Z" \
  ghcr.io/strio-dev/bale:latest \
  push ./restored mirror.example.com/release:bundle \
  --delivery bale.delivery.yaml --dry-run
```

Use new output paths. Replace the destination, configure credentials below, then remove `--dry-run` to publish. See [offline packaging](../offline.md) to move the archive between machines.

## Registry credentials

Create a Docker-compatible credential directory with your registry login tool, then mount it read-only:

```sh
podman run --rm --userns=keep-id --user "$(id -u):$(id -g)" \
  -v "$PWD:/work:Z" -v "$HOME/.config/bale/registry-auth:/registry:ro,Z" \
  -e DOCKER_CONFIG=/registry \
  ghcr.io/strio-dev/bale:latest \
  push ./restored mirror.example.com/release:bundle --delivery bale.delivery.yaml
```

The example expects `$HOME/.config/bale/registry-auth/config.json`. The image has no credential helpers, so the file must contain usable credentials. Keep it outside bundles and Git. For a private CA, mount its PEM file read-only and set `SSL_CERT_FILE` to that path. See [registry access](../registry.md#authentication-and-transport).

## Docker

On Linux, replace `podman` with `docker` and omit `--userns=keep-id`. Keep `--user "$(id -u):$(id -g)"` for host-owned output. The image defaults to UID/GID `65532:65532` and supports arbitrary UIDs with writable `/tmp`, `/work` and `/cache`.

## Build locally

From a checkout with [Nix set up](../contributing.md):

```sh
just container-build
podman run --rm oci:result-container:nix --version
```

Nix builds the current platform's OCI layout without a container runtime. Its local tag is `nix`; the version label and CLI report the build version.
