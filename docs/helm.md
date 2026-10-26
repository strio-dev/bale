# Transfer and deploy Helm charts

Bundle a chart with its default images, deliver it to a registry, then deploy with `helm bale`. The chart archive stays unchanged.

Capture needs Bale and registry access. Deployment needs Helm 4 and the native Bale Helm plugin. Follow the [offline Helm example](examples/helm-offline/README.md) for a complete removable-media workflow.

## Publish and configure discovery

Tagged releases publish the discovery package below. Replace `VERSION_OCI` with a release tag, encoding `+` as `_`, and select your chart in `bale.yaml`. For custom registries, [build and publish](development/releases.md#publish-artifacts) your own package.

```yaml
include:
  - ref: registry.example.com/charts/web:1.4
    plugin:
      ref: ghcr.io/strio-dev/bale/plugins/helm-discovery:VERSION_OCI
      input:
        schemaVersion: 1
        release: web
        namespace: production
        context: production
      blobs:
        chart:
          mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
```

Bale renders the chart in its Wasm sandbox and includes the discovered images. It needs neither Helm nor a container runtime on the capture machine. Package every declared subchart under `charts/`, including disabled dependencies. Hooks are rendered and inspected, not executed.

This captures chart-default images. Include deployment-specific images as ordinary recipe entries, then set their destination references in deployment values. See the [discovery reference](reference/helm-discovery.md#request-fields) for render inputs and supported workloads.

## Use delivered images in Helm

The native plugin includes the `helm bale` command and its private post-renderer. Install the released package matching the deployment machine, or carry it in the bundle below. Use `linux-arm64` on ARM64:

```sh
helm plugin install oci://ghcr.io/strio-dev/bale/plugins/bale:VERSION_OCI-linux-amd64 --verify=false
```

### Package and transfer

Add the released native package to the recipe's existing `include` list. [Local builds and custom publication](development/releases.md#build-integrations) remain available:

```yaml
  - ghcr.io/strio-dev/bale/plugins/bale:VERSION_OCI-linux-amd64
```

Choose the package matching the receiving machine. Helm does not select native plugins from a multiarchitecture index. Keep the final repository name `bale` when publishing and delivering it.

```sh
bale create -f bale.yaml -t release:v1 --output-delivery bale.delivery.yaml
```

Creation reads the chart and discovery plugin, locks the image digests under `release:v1` in the local store and keeps ordinary image payloads at their source locations until push or save. In the generated delivery file, set the chart's path to `charts/web` and the native plugin's path to `plugins/bale`. Review the remaining [delivery paths](transfers.md#export-delivery-paths), then push:

```sh
bale push release:v1 mirror.example.com/release:bundle --delivery bale.delivery.yaml
```

On the receiving machine, install into a Helm environment without an existing `bale` plugin:

```sh
helm plugin install oci://mirror.example.com/release/plugins/bale:VERSION_OCI-linux-amd64 --verify=false
```

Packages are unsigned, hence `--verify=false`. Use your verification policy for signed packages.

### Render or deploy

Use an explicit chart tag or digest:

```sh
helm bale template web oci://mirror.example.com/release/charts/web:1.4 \
  --namespace production
helm bale upgrade web oci://mirror.example.com/release/charts/web:1.4 \
  --install --namespace production --create-namespace --wait
```

Put Helm flags after the chart. Pass deployment values with `--values`, using destination references for custom images. The plugin resolves and pins the chart, reads its destination records, then relocates rendered image references. It uses Helm's post-renderer slot, so an additional `--post-renderer` is unsupported.

Use ordinary Helm commands for rollback, history and uninstall. Stored release manifests already contain the relocated references. For GitOps, use the [Flux adapter](flux.md).

### Private registry access

Log in with Helm and pass CA trust on each operation:

```sh
export HELM_CONFIG_HOME="$PWD/helm-config"
export HELM_REGISTRY_CONFIG="$HELM_CONFIG_HOME/registry/config.json"
helm registry login mirror.example.com --username deploy \
  --password-stdin --ca-file registry-ca.crt < registry-password
helm plugin install oci://mirror.example.com/release/plugins/bale:VERSION_OCI-linux-amd64 \
  --verify=false --ca-file registry-ca.crt
helm bale upgrade web oci://mirror.example.com/release/charts/web:1.4 \
  --install --namespace production --create-namespace --wait --ca-file registry-ca.crt
```

Keep both credential-location variables aligned: Helm 4.3.0's plugin installer does not honor a custom `--registry-config` alone. Workload pulls need separate Kubernetes credentials and node CA trust; see [private registry configuration](flux.md#private-registry-configuration).

### Selection and validation

When several captures match a chart, choose one with selectors before the action:

```sh
helm bale --context production upgrade web \
  oci://mirror.example.com/release/charts/web:1.4 --install --namespace production --create-namespace
```

See the [consumer reference](reference/helm-discovery.md#select-a-capture) for all selectors and validation rules.

### Image inventory

The renderer relocates supported workloads and hooks, leaving valid unmapped images unchanged. See [supported images and limits](reference/helm-discovery.md#supported-images-and-limits).

### Offline filter

To relocate an already-rendered stream without registry access, use the [offline filter](reference/helm-discovery.md#filter-an-existing-stream).

## Reuse after transfer

Select delivered chart and discovery-plugin references in the next recipe. Supply delivered image references in discovery values and keep the `chart` blob binding. The new capture generates a fresh declaration; see [plugin reuse](plugins.md#reuse-a-delivered-plugin).

## Generate a declaration manually

Use the [manual discovery workflow](reference/helm-discovery.md#generate-a-declaration-manually) to preview or attach a declaration outside bundle creation.

## Offline build

See the [offline build reference](reference/helm-discovery.md#offline-build) for the Nix-managed Helm patch and upgrade requirements.
