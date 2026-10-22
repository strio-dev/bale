# Bale

Bale bundles OCI images and artifacts for registry and offline transfer, preserving their original digests, multiarchitecture content and declared dependencies.

## Install

Install a published Linux AMD64 or ARM64 release:

```sh
curl -fsSL https://bale.strio.dev/install.sh | sh
```

See [getting started](docs/getting-started.md) for exact versions, a custom install prefix and source builds.

## Quick start

Save this as `bale.yaml`:

```yaml
include:
  - registry.example.com/apps/api:stable
```

Create a bundle, pull it to a local OCI layout, then deliver it:

```sh
bale create -f bale.yaml registry.example.com/bundles/release:v1
bale pull registry.example.com/bundles/release:v1 ./release
bale push ./release mirror.example.com/releases/v1:bundle
```

See [getting started](docs/getting-started.md) for setup and a walkthrough. The [documentation](docs/index.md) covers recipes, inspection, dependency declarations, registry behavior and the Go SDK.

## Development

Development uses the pinned Nix environment. See [contributing](CONTRIBUTING.md) for source builds, tests and documentation checks.

Bale is licensed under the [MIT License](LICENSE).
