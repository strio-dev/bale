# Getting started

Bale runs on Linux AMD64 and ARM64. Capturing artifacts needs registry access; a container runtime is not required.

## Install

```sh
curl -fsSL https://bale.strio.dev/install.sh | sh
bale --version
```

This installs the latest stable release. For a specific version or Nix, see [other installation options](#other-installation-options).

## Run in a container

Use the published Bale image with Podman or Docker:

[Run Bale in a container](guides/container.md){ .md-button .strio-container-link }

## Create your first bundle

[**Capture and carry a container image**
<span>Bundle BusyBox, inspect its contents, and restore it from an offline archive.</span>
<span class="strio-tutorial__action">Start tutorial</span>](tutorials/first-bundle.md){ .strio-path .strio-tutorial }

See [how bundles work](concepts/bundles.md) for the capture and delivery lifecycle, or [transfer artifacts](transfers.md) to store a bundle in a registry.

## Other installation options

### Install a specific version

Pass a published tag from [GitHub releases](https://github.com/strio-dev/bale/releases), replacing `VERSION` below:

```sh
curl -fsSL https://bale.strio.dev/install.sh | sh -s -- VERSION
```

The installer verifies checksums and installs under `/usr/local`, using `sudo` when needed. To use a writable directory instead:

```sh
curl -fsSL https://bale.strio.dev/install.sh | BALE_INSTALL_PREFIX="$HOME/.local" sh
```

Add `$HOME/.local/bin` to `PATH` if needed.

### Install with Nix

With Nix flakes enabled:

```sh
nix profile add github:strio-dev/bale
bale --version
```

This builds the repository's default branch. See [contributing](contributing.md) for source builds and development setup.
