# Getting started

Install Bale on Linux AMD64 or ARM64, then capture your first bundle. Bale needs registry access to read artifacts; it does not need a container runtime.

## Install a release

Install the latest stable Linux binary:

```sh
curl -fsSL https://bale.strio.dev/install.sh | sh
bale --version
```

The installer verifies checksums and installs under `/usr/local`, using `sudo` when needed. To choose a writable prefix:

```sh
curl -fsSL https://bale.strio.dev/install.sh | BALE_INSTALL_PREFIX="$HOME/.local" sh
```

Add `$HOME/.local/bin` to `PATH` if needed. To select a version, append `-s -- TAG` to `sh`, replacing `TAG` with an exact tag from [GitHub releases](https://github.com/strio-dev/bale/releases).

## Create your first bundle

[**Capture and carry a container image**
<span>Bundle BusyBox, inspect its contents, and restore it from an offline archive.</span>
<span class="strio-tutorial__action">Start tutorial</span>](tutorials/first-bundle.md){ .strio-path .strio-tutorial }

Read [how bundles work](concepts/bundles.md) for the recipe, captured snapshot and delivery file, or [transfers](transfers.md) to use a registry as the bundle store.

## Install with Nix

With Nix flakes enabled:

```sh
nix profile add github:strio-dev/bale
bale --version
```

This builds the repository's default branch.

## Build from source

From a checkout:

```sh
nix develop path:.
just build
./result-bale/bin/bale --version
```

Inside the development shell, `bale` runs the current source. See [contributing](contributing.md) for direnv and development checks.
