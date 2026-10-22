# Getting started

Bale moves OCI content between registries through a self-contained bundle. Create directly into a local OCI layout or store a bundle in a registry. Save a portable archive for offline transfer; push chooses the final registry.

## Install a release

The installer downloads a published GitHub release for Linux AMD64 or ARM64, verifies its checksum and installs Bale under `/usr/local`. It uses `sudo` there when needed:

```sh
curl -fsSL https://bale.strio.dev/install.sh | sh
```

To install without elevated permissions, choose a writable prefix:

```sh
curl -fsSL https://bale.strio.dev/install.sh | BALE_INSTALL_PREFIX="$HOME/.local" sh
```

Add `$HOME/.local/bin` to `PATH` if needed. To select an exact published tag, pass it to the script, for example:

```sh
curl -fsSL https://bale.strio.dev/install.sh | sh -s -- 1.2.3
```

Replace `1.2.3` with the release you want. Installation requires a published release; source builds work before the first release is available.

## Build from source

From a checkout, enter the pinned Nix environment and build Bale:

```sh
nix develop path:.
just build
./result-bale/bin/bale --version
```

Inside the development shell, `bale` runs the current checkout's source. The packaged executable is `result-bale/bin/bale`. See [contributing](contributing.md) for direnv and development checks.

## Write a recipe

Save this as `bale.yaml`, replacing the example references with images or artifacts you can read:

```yaml
include:
  - registry.example.com/apps/api:stable
  - registry.example.com/apps/worker:1.4
```

Each source needs an explicit registry and tag or digest. Images, Helm OCI charts and other OCI artifacts use the same inclusion syntax. Bale preserves their content bytes; [discovery plugins](plugins.md) can declare application dependencies during capture. Consumer reference rewriting belongs in integrations.

## Capture and transfer

```sh
bale create --layout -f bale.yaml ./release --output-delivery bale.delivery.yaml
bale inspect --layout ./release
bale save ./release -o release.tar
```

Carry `release.tar` and the delivery file to the receiving environment:

```sh
bale load -i release.tar ./restored
bale push ./restored mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml --dry-run
bale push ./restored mirror.example.com/releases/v1:bundle \
  --delivery bale.delivery.yaml
```

Only read access to the sources is needed to create the local bundle. Save and load preserve its OCI graph and digests without network access. Edit delivery paths before pushing if individual artifacts need specific locations. Follow the complete [offline packaging walkthrough](offline.md).

You can also [create a registry bundle](transfers.md#create-a-bundle) and pull it into a layout. Both paths produce the same portable bundle format. The destination registry is chosen at push time.

Registry commands use Docker credential configuration, including `DOCKER_CONFIG`. For an HTTP registry, add `--plain-http localhost:5000` with its exact host. Transfer commands print bundle digests; inspection returns JSON. Use `bale COMMAND --help` for options.

Read [recipes](recipes.md) to select multiple tags and dependencies, [transfers](transfers.md) for refresh and destination mappings, and [registry behavior](registry.md) before configuring garbage collection.
