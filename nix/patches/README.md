# Helm offline rendering patch

`helm-offline.patch` targets `helm.sh/helm/v4` v4.3.0, upstream commit
`bec5b06ed841fe5269972d864d5177944fd5970f`, with Kubernetes modules v0.37.0.
It keeps Helm's chart loader, template engine, Sprig functions, dependency
processing and schema validation while separating cluster-client imports from
offline rendering.

`nix/helm-plugin.nix` applies the patch through `buildGoModule.modPostBuild` after
vendoring. The fixed `vendorHash` verifies the patched dependency tree. The Wasm
build selects `helm_offline`; default native checks retain the client-aware
APIs. Builds need no fork, Python, source-rewriting script or module-cache edits.
Every changed or added Helm source file carries a prominent
“Modified for Bale offline rendering” comment; Helm's Apache-2.0 license and
original source notices are preserved.

Packaged CLI, native consumers and the Wasm plugin include `third-party`
notices under their `share/licenses/bale*` directories. These contain the pinned
Go toolchain's LICENSE/PATENTS, vendored `modules.txt` and dependency license
and notice files at their module-relative paths. Native Helm archives also
carry these notices beside `plugin.yaml`. The Wasm package includes the applied
patch and upstream Helm LICENSE under its `third-party` directory. The inventory
can include dependencies used only for building or testing; it does not change
the licenses applicable to individual components.

The patch changes three import boundaries:

- Version metadata imports `client-go/pkg/version` instead of the Kubernetes
  clientset.
- Offline capabilities use 57 API-version strings captured from the pinned
  stock dependencies. The list preserves the captured ordering and version
  priority within each API group. Stock group order can vary between runs;
  the snapshot does not promise a universal stock ordering. KubeVersion and
  HelmVersion still use Helm's existing derivation.
- Cluster-aware engine constructors and lookup code compile without
  `helm_offline`. Offline rendering keeps Helm's existing empty `lookup`
  behavior for an engine without a cluster client.

The public Go module files still select unmodified Helm. Direct `go build`
commands therefore produce the larger module unless they use the patched Nix
dependency tree.

## Updating Helm

Review the patch against the new Helm and Kubernetes versions. Capture default
capabilities from those unmodified dependencies and refresh the offline list.
Compare stock and offline API membership, ordering within each group,
KubeVersion and HelmVersion; preserve the captured group order without assuming
it is stable between stock executions. Run the default native checks again to
verify the client-aware APIs, then rebuild the Nix package and run its Wasm
integration checks. Update `vendorHash` only after reviewing the dependency and
patch changes.

The import-boundary problem is tracked in Helm's
[lightweight renderer proposal](https://github.com/helm/helm/issues/32468).
The version-import change follows the approach in
[Helm PR #32469](https://github.com/helm/helm/pull/32469).
