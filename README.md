# Bale

Bale bundles OCI images and artifacts for registry and offline transfer.

## Install

```sh
curl -fsSL https://bale.strio.dev/install.sh | sh
bale --version
```

See [getting started](https://bale.strio.dev/getting-started/) for requirements, exact versions, the container image and Nix.

## Quick start

Save this as `bale.yaml`:

```yaml
include:
  - docker.io/library/busybox:1.37
```

Capture the image and save an offline archive:

```sh
bale create --layout -f bale.yaml ./release
bale save ./release -o release.tar
```

Follow the [first-bundle tutorial](https://bale.strio.dev/tutorials/first-bundle/) to inspect, restore and deliver it. The [documentation](https://bale.strio.dev/) covers registry transfers, Helm, Flux and the Go SDK.

## Development

Development uses the pinned Nix environment. See [contributing](CONTRIBUTING.md) for source builds, tests and documentation checks.

Bale is licensed under the [MIT License](LICENSE).
