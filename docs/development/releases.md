# Packaging and releases

Build and publishing commands use pinned Nix tools. Registry credentials are supplied at publication time.

## Build integrations

| Command | Output |
| --- | --- |
| `just helm-plugin-build` | Raw discovery module for local development. |
| `just helm-plugin-package` | Discovery archive with metadata, module and license files. |
| `just helm-plugin-image-build` | Discovery OCI layout. |
| `just helm-consumer-build` | Native Helm plugin directory. |
| `just helm-consumer-package` | Installable native Helm plugin archive. |
| `just helm-consumer-image-build` | Native Helm plugin OCI layout. |
| `just flux-service-build` | Flux adapter executable. |
| `just flux-image-build` | Flux adapter OCI image layout. |
| `just flux-chart-build` | Packaged Flux adapter chart. |

Build all finished artifacts for the current Linux platform:

```sh
just distribution-build
```

`result-distribution/` contains archives under `archives/`, OCI layouts under `oci/`, and the development module with license files under `helm-discovery/`. Native Helm archives have platform-specific directories. The flake supports AMD64 and ARM64.

## Publish artifacts

To publish a local build to a custom registry:

```sh
nix run --no-update-lock-file path:.#publish -- \
  helm-discovery registry.example.com/plugins/helm:1
```

Replace the selector and destination for each artifact:

| Selector | Destination example |
| --- | --- |
| `bale` | `registry.example.com/images/bale:dev` |
| `helm-discovery` | `registry.example.com/plugins/helm:1` |
| `helm-consumer` | `registry.example.com/plugins/bale:0.0.0-linux-amd64` |
| `bale-flux` | `registry.example.com/images/bale-flux:dev` |
| `bale-flux-chart` | `oci://registry.example.com/charts` |

OCI publication uses ORAS copy options, including `--to-registry-config` and `--to-ca-file`. Chart publication uses Helm options such as `--ca-file`; Helm appends the chart name and version. The native plugin's repository name must end in `bale`. Use an architecture-specific tag for each native build.

## Release a version

After checks pass, push an unprefixed version tag such as `1.2.3` or `1.2.3-rc.1`. CI publishes Linux AMD64/ARM64 CLI archives and `SHA256SUMS` on GitHub, plus the complete OCI distribution:

| Artifact | Published reference |
| --- | --- |
| Bale image | `ghcr.io/strio-dev/bale:VERSION_OCI` |
| Flux adapter image | `ghcr.io/strio-dev/bale-flux:VERSION_OCI` |
| Helm discovery plugin | `ghcr.io/strio-dev/bale/plugins/helm-discovery:VERSION_OCI` |
| Native Helm plugin | `ghcr.io/strio-dev/bale/plugins/bale:VERSION_OCI-linux-ARCH` |
| Flux bootstrap chart | `oci://ghcr.io/strio-dev/bale/charts/bale-flux` with `--version VERSION` |

`VERSION` is the exact release tag; `VERSION_OCI` replaces `+` with `_`. Both images support AMD64 and ARM64. Native Helm packages use separate `amd64` and `arm64` tags because Helm does not select them from an index. Discovery and the chart publish once per release; chart metadata and binaries retain the exact version.

CI publishes the GitHub release after the OCI artifacts succeed. Only stable releases update image `latest` aliases. Reruns preserve published version tags and keep older releases from replacing the current `latest` images. The publishing command above remains available for custom registries.
