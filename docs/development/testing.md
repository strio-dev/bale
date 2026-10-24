# Test suites

Run focused checks for the code you change, then `just check`. The root library, Helm discovery producer (`plugins/helm`) and native consumers (`plugins/helm-relocate`) are separate Go modules. `just test`, `just vet` and `just lint` cover all three.

## Container image

`just container-build` produces `result-container/`, an OCI image layout. Native checks validate its digests, architecture, static executable, CA bundle, license files and non-root filesystem permissions. See [local container builds](../guides/container.md#build-locally) to run it with Podman.

## Container acceptance

```sh
just container-acceptance
```

This NixOS VM check requires **Linux AMD64 with readable and writable `/dev/kvm`**. It runs the packaged image in rootless Podman, using both its default user and an arbitrary user. No host container runtime is required; Nix supplies the image, registries and tools.

Against both Distribution and Zot, it captures nested bundles, dependencies, multiarchitecture images and portable plugins; restores an archive after stopping its sources; delivers it; and retrieves the delivered graph. The transport cases check registry credentials and private-CA trust. Skipped cases fail the check. VM output retains the test logs.

This check is included in `just check` and default CI on AMD64. ARM64 CI builds and validates the image without running this VM.

## Plugin and consumer builds

| Command | Checks or output |
| --- | --- |
| `just helm-plugin-build` | Portable Helm discovery module. |
| `just helm-plugin-package` | Discovery module, metadata and license files in a plugin archive. |
| `just helm-plugin-image-build` | Discovery archive packaged as an OCI artifact. |
| `just helm-plugin-check` | Built module through the offline capture workflow. |
| `just helm-consumer-build` | Native Helm plugin directory, with module tests, vet and lint. |
| `just helm-consumer-package` | Standard Helm plugin archive, checked with Helm's installer. |
| `just helm-consumer-image-build` | Native archive packaged as a Helm OCI plugin layout. |
| `just flux-service-build` | Flux adapter, with module tests, vet and lint. |
| `just flux-image-build` | OCI image layout, including static linkage, CA roots and store-link checks. |
| `just flux-chart-build` | Linted and packaged adapter chart. |

Both discovery checks are required for producer changes and included in `just check`. See [Helm discovery](../reference/helm-discovery.md) and [consumer packaging](../helm.md#use-delivered-images-in-helm).

## Distribution output

```sh
just distribution-build
```

`result-distribution/` collects finished artifacts for the current Linux platform:

- `archives/`: CLI and discovery archives, native Helm plugin archive under its platform directory, and Flux chart.
- `oci/`: CLI, Helm discovery, native Helm plugin and Flux service layouts.
- `helm-discovery/`: portable module and its license notices.

Nix uses pinned tar/gzip for the discovery archive, Helm's packager for the native Helm plugin and ORAS for OCI layouts. Native AMD64 and ARM64 CI checks build the distribution output. Packaging checks compare archive contents and OCI descriptors with the original build inputs; the native archive is installed with stock Helm. Registry acceptance transfers and installs the built archive.

Discovery metadata, its portable module and license files travel together inside the archive layer. The package uses Bale's WASI execution contract; packaging does not require Helm's Extism runtime.

Publish a built artifact with `nix run --no-update-lock-file path:.#publish -- ARTIFACT DESTINATION [publisher options]`. Select `bale`, `helm-discovery`, `helm-consumer`, `bale-flux` or `bale-flux-chart`; see the [Helm](../helm.md#package-and-transfer) and [Flux](../flux.md#build-and-deliver-the-adapter) guides for destinations. Registry credentials remain runtime inputs. Tag-release CI publishes the CLI archives and image; integration artifacts use this separate publishing command.

## Registry acceptance

```sh
just registry-acceptance
```

The packaged CLI runs against native Distribution 3.1.2 and Zot 2.1.22 processes using temporary storage and loopback ports. No container runtime is required. This Nix check is included in `just check` and CI.

It covers capture, refresh, archive restoration, delivery and locked retrieval with source registries stopped. Cases include nested bundles, multiarchitecture images, declarations, portable discovery plugins and native Helm installation from delivered OCI packages. Publication failure/retry and tag conflicts check partial reports and completion ordering.

An HTTPS/Basic-auth frontend checks client credentials and certificate trust. It does not test the registries' own authentication configuration. Registry cases render charts; live workloads belong to the Kubernetes suite.

## Kubernetes acceptance

```sh
just kubernetes-acceptance
```

This optional suite requires **Linux AMD64 with readable and writable `/dev/kvm`**. The NixOS test VM uses four CPUs, 4 GiB RAM and a sparse 16 GiB disk. Nix fetches pinned tools and images before startup; containerd runs inside the VM. No host container runtime or Kubernetes context is needed.

The suite captures and archives content, stops source registries, then delivers to a private-CA HTTPS registry. Native Helm and real Flux Operator deploy unchanged charts, exercise explicit custom-image overrides, upgrades and rollback, and compare running image digests. Failure cases cover credentials, CA trust, ambiguous records and registry outages. The upstream-chart case uses pinned Podinfo artifacts.

To retain Kubernetes objects, logs, JSON results and JUnit output, choose a new directory:

```sh
nix run --no-update-lock-file path:.#kubernetes-acceptance -- --output result-kubernetes-acceptance
```

Evidence is under `evidence/`, with `junit.xml` alongside it. Without `--output`, temporary results are removed on exit. The driver cleans up the VM on success or failure.

This suite stays outside `just check` and default CI. Manual CI can opt in through the `kubernetes` input on a runner with KVM. Controller/tool pins live in `nix/kubernetes-inputs.nix`; updates require refreshed hashes and a full run. See [private registry configuration](../flux.md#private-registry-configuration) for the separate consumer and node trust settings.
