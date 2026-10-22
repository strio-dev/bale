# Run Bale in a container

Use the Bale image to capture and deliver bundles with Podman or Docker. It includes the CLI, CA roots and license notices for Linux AMD64 and ARM64.

## Check the CLI

```sh
podman run --rm ghcr.io/strio-dev/bale:latest --help
```

Replace `latest` with a published release tag to pin a version. Prereleases have versioned tags; stable releases update `latest`. In image tags, replace a release version's `+` with `_`.

## Capture a bundle

Save the [first-bundle recipe](../tutorials/first-bundle.md#write-a-recipe) as `bale.yaml` in a working directory. Mount that directory and an optional cache volume:

```sh
podman run --rm --userns=keep-id --user "$(id -u):$(id -g)" \
  -v "$PWD:/work:Z" -v bale-cache:/cache \
  ghcr.io/strio-dev/bale:latest \
  create --layout -f bale.yaml ./release --output-delivery bale.delivery.yaml
```

`/work` is the working directory; outputs appear in your host directory. The user and namespace options give them your host ownership. `:Z` labels the bind mount for SELinux. The optional `/cache` volume retains compiled discovery plugins between runs; keep each cache volume private to one effective user.

No privileged mode, container socket or nested runtime is needed. Plugins run inside Bale. Without a cache volume, their compiled cache lasts only for that container.

## Save, restore and deliver

Use the same workspace mount for subsequent commands:

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

Replace the destination with a registry you can write to, configure credentials below, then remove `--dry-run` to publish. Layouts, archives and exported delivery files require new paths. See [offline packaging](../offline.md) for moving the archive between machines.

## Registry credentials

Mount a Docker-compatible credential directory read-only and set `DOCKER_CONFIG` to its container path. For example, create `$HOME/.config/bale/registry-auth/config.json` with your registry login tool, then mount that directory separately from the workspace:

```sh
podman run --rm --userns=keep-id --user "$(id -u):$(id -g)" \
  -v "$PWD:/work:Z" -v "$HOME/.config/bale/registry-auth:/registry:ro,Z" \
  -e DOCKER_CONFIG=/registry \
  ghcr.io/strio-dev/bale:latest \
  push ./restored mirror.example.com/release:bundle --delivery bale.delivery.yaml
```

The image has no credential-helper executables; the mounted config must contain usable registry credentials. Keep it out of bundles and Git. For a private CA, mount its PEM file read-only and set `SSL_CERT_FILE` to that path. [Registry configuration](../registry.md#authentication-and-transport) also covers exact-host HTTP access.

## Docker

On Linux, use the same commands with `docker` and omit `--userns=keep-id`, keeping `--user "$(id -u):$(id -g)"`. The image defaults to UID/GID `65532:65532`; both engines can override it. `/tmp`, `/work` and `/cache` are writable, and `HOME=/tmp` permits arbitrary UIDs.

## Build locally

From a Bale checkout with [Nix set up](../contributing.md):

```sh
just container-build
podman run --rm oci:result-container:nix --version
```

The build needs no container runtime and produces an OCI layout for the host architecture. Podman is needed only to run it. Local builds use the constant layout tag `nix`; the image version label and CLI report the build's Bale version.
