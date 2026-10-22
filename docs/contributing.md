# Contributing

Enter `nix develop` or enable direnv with `direnv allow`. Run `just fmt`, `just test`, `just vet`, and `just lint`; `just check` verifies the local source, workflows, and documentation. `just release-check` checks the Git source after staging changes.

The development `bale` command runs the current checkout's source, including edits. `nix run path:. -- --version` runs the packaged CLI. Build with `just build`; the executable is `result-bale/bin/bale`.

Run `just registry-acceptance` (or `nix run path:.#registry-acceptance`) for the complete packaged CLI workflow against native Distribution 3.1.2 and Zot 2.1.22 processes. It captures and refreshes a bundle, then stops the source before comparing snapshots, preserving a custom delivery path, previewing and pushing to the destination. It verifies delivered digests, tags, attachments and a fresh locked pull with the holding registry also stopped. Registries use temporary storage and loopback ports; no container runtime is required. This test is separate from `just check` and also runs in CI; ordinary Go tests skip it unless the acceptance executable paths are configured.

It also captures directly into a local layout with read-only source access, saves an archive, removes the original layout, stops the source, restores and delivers to each registry. The offline case covers nested bundles, multiarchitecture content, selected declarations and portable plugin content.

The Helm case publishes the Nix-built discovery module, a packaged chart and fixture OCI image graphs. A recipe selects the registry plugin so creation discovers and captures the chart's images automatically. After the source stops, the test saves and restores the bundle, delivers it, and checks chart bytes, image digests and restored plugin tags. It then uses the delivered plugin and chart for a new capture with an empty compilation cache. This checks discovery and transfer without running the chart's workloads.

The same case packages the native Helm CLI integration in Helm's standard OCI format and transfers it in the bundle. After the source stops and local transport copies are removed, stock Helm installs it from the destination registry. `helm bale` resolves the delivered chart's attachments automatically and invokes its private renderer. The test checks main, init-container and subchart-hook image relocation, client dry-run installation, idempotence, valid unmapped images and explicit selection of ambiguous records. The offline filter also runs with an empty executable search path and `GOMAXPROCS=4`.

The same command checks failed graph, destination-record, alias and completion publication followed by an identical retry. It verifies partial JSON reports, withheld completion tags, content reuse, discoverable attachments and preservation of conflicting alias, completion and managed inventory tags without writes. An HTTPS/Basic-auth frontend over each native registry checks Docker credentials and certificate trust; it does not qualify the registries' own authentication or TLS configuration.

The Helm discovery plugin has its own Go module under `plugins/helm`. Root-module `go test ./...` does not test it. From the development shell, run its native tests and vet separately:

```sh
(
    cd plugins/helm
    go test ./...
    go vet ./...
)
```

Run `just helm-plugin-build` to build the portable module and `just helm-plugin-check` to check the built Wasm through the offline bundle workflow. Both checks are required for Helm plugin changes; `just check` includes them. See [Helm discovery](helm.md) for the packaged module path and request format.

The native consumers share a separate module under `plugins/helm-relocate`; `just test`, `just vet` and `just lint` include it. `just helm-consumer-build` builds the platform-specific Helm plugin package; `just flux-service-build` builds the Flux Operator adapter. Both builds run module tests, vet and lint. Use `just registry-acceptance` for the actual Helm and registry workflow; see [Helm consumption](helm.md#use-delivered-images-in-helm) and [Flux](flux.md) for configuration and integration boundaries.

`just flux-image-build` packages the adapter as an OCI image layout without a container runtime; the build verifies static linkage, copied CA roots and the absence of Nix store links. `just flux-chart-build` lints and packages `charts/bale-flux`. Registry acceptance publishes those real artifacts, discovers the image through the shipped plugin, stops the source, and verifies delivered chart bytes and Helm bootstrap relocation against both registries. These registry checks render workloads without running them.

## Kubernetes acceptance

Run `just kubernetes-acceptance` for the optional live Helm and Flux workflow. It uses the standard NixOS VM test driver on **Linux AMD64 with readable and writable `/dev/kvm`**. The VM has four CPUs, 4 GiB of memory and a sparse 16 GiB disk. Nix builds or fetches pinned tools, controller images and installation manifests before starting it. The test uses containerd inside the disposable VM; it needs no host Kubernetes context, Docker, Podman or kind.

The test captures bundles, stops the source registry, and delivers to an authenticated HTTPS destination with a private CA. Helm, the resolver, Flux source-controller and workload pulls each use their configured credentials and CA trust. Assertions reject untrusted certificates and bad credentials, including Pod pulls with cached image blobs, and verify recovery with corrected Secrets.

Capture uses the packaged Bale CLI and an OCI-hosted Helm discovery plugin selected in a recipe. It edits the generated delivery file, saves and removes the original layouts, then loads and pushes the archives with the source registry offline. Helm installs its transferred native plugin from the destination. Source fixtures publish raw artifacts; the CLI discovers parent, init-container and bundled subchart-hook dependencies from chart defaults. A custom image is included explicitly and selected through destination deployment values without changing discovery inputs. The service bootstrap likewise captures its image explicitly and uses plain Helm with destination values. See the [matching authoring example](examples/helm-offline/README.md).

Live checks cover actual main, init-container and hook image digests, independent Helm releases, upgrade/rollback and Pod replacement, unchanged charts, and packaged service bootstrap. Both Helm and Flux accept the custom destination image while still relocating chart-default init containers and hooks. Real Flux Operator reconciles application A→B→A over HTTPS with bearer authentication. Helm and resolver credential failures preserve the existing release; ambiguous metadata retains prior inputs and the running Pod. Health checks must work during a destination registry outage.

The upstream-chart case uses the unchanged Podinfo 6.9.4 archive in `tests/integration/testdata/helm` and real upstream Linux AMD64 images. Nix fetches five images by fixed digest and archive hash before the VM starts: Podinfo 6.9.4 and 6.9.3, plus the chart's three test-hook images. An isolated native registry mirror serves the pinned images under their original `ghcr.io` and `docker.io` names inside the guest; public registry tags are not changed. Capture discovers chart defaults without deployment values and includes Podinfo 6.9.3 as an ordinary recipe image. Both the mirror and source registry stop before archive load, delivery and installation. Native Helm and Flux deploy 6.9.4, select delivered 6.9.3 through deployment-only values, then roll back or restore 6.9.4. Checks compare Pod image IDs and the application's `/version` response. This case qualifies Linux AMD64, alongside the existing synthetic workload, hook and failure checks.

To retain Kubernetes objects, logs, a JSON summary and JUnit results, choose an empty output directory:

```sh
nix run --no-update-lock-file path:.#kubernetes-acceptance -- --output result-kubernetes-acceptance
```

Evidence is written under `result-kubernetes-acceptance/evidence`, with `junit.xml` alongside it. Without `--output`, temporary output is removed on exit. The NixOS driver cleans up its VM on success or test failure. This command stays separate from `just check` and default CI. A manual CI run can opt in through the `kubernetes` input; its runner must expose readable and writable `/dev/kvm`.

`nix/kubernetes-inputs.nix` pins Flux 2.9.6, source-controller 1.9.6, helm-controller 1.6.5 and Flux Operator 0.61.0. Updating these inputs requires refreshing their fixed hashes and running the full test. Controller images are imported from Nix-built OCI archives and use digest references with pull policy `Never`.

For deployment credentials, CA mounts and node trust, see the [private registry configuration](flux.md#private-registry-configuration).

Use `just scan` for the pinned Semgrep rules and `just vuln` for Go vulnerability analysis. `just docs-build` builds Zensical documentation strictly. Keep documentation limited to available behavior.

Semgrep excludes raw Helm Go templates, which are not standalone YAML. The Flux chart check validates them with `helm lint --strict` and `helm template`.

Preview the documentation with `just docs-serve`.

## CI and releases

CI groups the Nix quality, documentation and native checks, and runs registry acceptance against Distribution and Zot. Native builds use Linux AMD64 and ARM64 runners. CodeQL and vulnerability checks cover all three Go modules; Semgrep scans the source and workflows. The documentation job includes isolated installer tests, then deploys the built site from `main` through GitHub Pages. Pages must be configured to use Actions and the `docs` environment in the destination repository.

Build a local CLI release archive with `just release-build`. The archive includes the static executable, Bale's license and Go/dependency notices. The release check extracts it and runs the CLI with an empty executable search path.

Push an unprefixed version tag such as `1.2.3` or `1.2.3-rc.1` after checks pass. CI takes the version from the tag, builds `bale-linux-amd64.tar.gz` and `bale-linux-arm64.tar.gz`, and publishes them with `SHA256SUMS`. Assets are assembled in a draft before publication; reruns leave an already published release unchanged. The installer supports the latest stable release or an exact tag. This workflow publishes CLI releases; the Helm plugin and Flux artifacts use their documented packaging commands.
