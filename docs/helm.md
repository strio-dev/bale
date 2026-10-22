# Transfer and deploy Helm charts

Capture a chart and its default images, deliver them to a registry, then use `helm bale` to deploy with the delivered image references. The chart archive stays unchanged.

Discovery runs during bundle creation in Bale's Wasm sandbox, using Helm's renderer. Creation needs neither an installed Helm command nor a container runtime. Deployment uses a separate native Helm 4 plugin. Packaging that native plugin also requires Helm 4 and ORAS on your `PATH`. For a complete removable-media workflow, follow the [Helm offline example](examples/helm-offline/README.md).

## Publish and configure discovery

From a Bale checkout with [Nix set up](contributing.md), build the discovery module and publish it to a repository you can write to:

```sh
nix build --no-update-lock-file path:.#helm-plugin --out-link result-helm-plugin
bale plugins push -f result-helm-plugin/lib/bale/plugins/helm.wasm registry.example.com/plugins/helm:1
```

Replace the example registry references with your own chart and plugin. Select discovery in `bale.yaml`:

```yaml
include:
  - ref: registry.example.com/charts/web:1.4
    plugin:
      ref: registry.example.com/plugins/helm:1
      input:
        schemaVersion: 1
        release: web
        namespace: production
        context: production
      blobs:
        chart:
          mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
```

During creation, Bale reads the chart layer, runs discovery and captures the plugin, declaration and discovered images. All declared subcharts must already be packaged under `charts/`, including disabled dependencies. Discovery inspects rendered hooks without executing them.

This recipe discovers the chart defaults. If deployment values introduce another image, add it as an ordinary recipe `include` entry, then write destination values after transfer. Bale does not generate deployment values or check that every override was captured. Optional discovery render inputs are described in the [request reference](reference/helm-discovery.md#request-fields).

## Use delivered images in Helm

Build the native plugin for the deployment machine. The flake supports Linux AMD64 and ARM64:

```sh
nix build --no-update-lock-file path:.#helm-consumer --out-link result-helm-consumer
helm plugin install result-helm-consumer/share/helm/plugins/bale
```

One installation includes the `bale` command and its private post-renderer. To carry the installer through the bundle too, package it before capture as below.

### Package and transfer

From the same checkout, package the native build with Helm 4 and publish it using Helm's OCI plugin format:

```sh
mkdir -p dist
helm plugin package result-helm-consumer/share/helm/plugins/bale --sign=false --destination dist
(
  cd dist
  oras push registry.example.com/plugins/bale:0.0.0-linux-amd64 \
    --artifact-type application/vnd.helm.plugin.v1+json \
    bale-0.0.0.tgz:application/vnd.oci.image.layer.v1.tar+gzip
)
```

The tag above identifies an AMD64 development build; use the matching architecture for your deployment machine. Helm does not select native plugins from a multiarchitecture index.

Add the published native package as another entry in the recipe's existing `include` list:

```yaml
  - registry.example.com/plugins/bale:0.0.0-linux-amd64
```

Create the bundle after adding that entry:

```sh
bale create --layout -f bale.yaml ./release --output-delivery bale.delivery.yaml
```

In the generated delivery file, choose `charts/web` for the chart and `plugins/bale` for the native plugin. Preserve the final repository name `bale`, which Helm uses to find its archive layer. [Delivery files](transfers.md#export-delivery-paths) control the remaining paths.

```sh
bale push ./release mirror.example.com/release:bundle --delivery bale.delivery.yaml
```

On the receiving machine, install the delivered package into a Helm environment without an existing `bale` plugin:

```sh
helm plugin install oci://mirror.example.com/release/plugins/bale:0.0.0-linux-amd64 --verify=false
```

The development package is unsigned, hence `--verify=false`; use your verification policy for signed packages.

### Render or deploy

Use the delivered chart's explicit tag or digest:

```sh
helm bale template web oci://mirror.example.com/release/charts/web:1.4 \
  --namespace production
helm bale upgrade web oci://mirror.example.com/release/charts/web:1.4 \
  --install --namespace production --wait
```

The grammar is `helm bale [Bale selectors] install|upgrade|template RELEASE OCI_CHART [Helm flags]`. Put Helm flags after the chart. Pass deployment values with `--values` as usual, using destination references for any custom images you included explicitly.

The adapter resolves the chart once, reads its [destination attachments](destinations.md) and passes the pinned chart to Helm. It keeps Helm's environment, streams and exit status. It occupies the post-renderer slot, so an additional `--post-renderer` is unsupported. Chart references need an explicit tag, digest or exact `--version`; ranges and implicit highest-version selection are unsupported.

Use ordinary Helm commands for rollback, history and uninstall. Stored release manifests already contain the relocated references. For GitOps, use the [Flux Operator adapter](flux.md).

### Private registry access

Use Helm's registry login and pass CA trust on each operation. With a locally provisioned password and CA:

```sh
export HELM_CONFIG_HOME="$PWD/helm-config"
export HELM_REGISTRY_CONFIG="$HELM_CONFIG_HOME/registry/config.json"
helm registry login mirror.example.com --username deploy \
  --password-stdin --ca-file registry-ca.crt < registry-password
helm plugin install oci://mirror.example.com/release/plugins/bale:0.0.0-linux-amd64 \
  --verify=false --ca-file registry-ca.crt
helm bale upgrade web oci://mirror.example.com/release/charts/web:1.4 \
  --install --namespace production --wait --ca-file registry-ca.crt
```

Helm 4.3.0's OCI plugin installer reads the default Helm credential location or Docker credentials; a custom `--registry-config` alone does not reach it. Keep `HELM_CONFIG_HOME` and `HELM_REGISTRY_CONFIG` aligned as above. The [pinned client implementation](https://github.com/helm/helm/blob/bec5b06ed841fe5269972d864d5177944fd5970f/pkg/registry/client.go#L87-L119) determines this behavior.

Attachment lookup and chart retrieval share `--plain-http`, `--ca-file`, `--cert-file`, `--key-file`, `--username`, `--password` and `--insecure-skip-tls-verify`. Prefer credential stores to passwords in arguments. Workload image pulls need separate Kubernetes credentials and node CA trust; see [private registry configuration](flux.md#private-registry-configuration).

### Selection and validation

The consumer requires one matching local attachment and one selected dependency resolution. When several captures match, use selectors before the action:

| Selector | Value |
| --- | --- |
| `--bundle` | Delivered bundle repository and digest. |
| `--source` | Original chart repository and digest. |
| `--capture` | Owning capture digest. |
| `--declaration` | Selected declaration digest. |
| `--context` | Declaration context string. |

```sh
helm bale --context production upgrade web \
  oci://mirror.example.com/release/charts/web:1.4 --install --namespace production
```

Selection never picks the newest attachment or unions captures. An explicitly selected empty declaration is valid; missing, disabled or unselected dependency observations fail before Helm starts.

### Image inventory

The renderer handles the [workloads supported by discovery](reference/helm-discovery.md#image-inventory), including hooks, init containers, ephemeral containers and nested Lists. It replaces mapped source selectors with pinned destination references and leaves valid unmapped references unchanged. Custom resources and arbitrary image fields require a consumer that understands their schema.

Malformed workloads, invalid images and invalid metadata fail without partial YAML. YAML anchors, aliases and merge keys on paths requiring mutation are rejected. Limits are 8 MiB of input YAML and 16 MiB of output. Attachment inspection permits 256 advertised referrers and 4 MiB of inventory and attachment reads; serialized records have a separate 4 MiB limit. The adapter passes records through a temporary `0600` file removed when Helm exits.

### Offline filter

To filter a stream that you rendered separately, inspect the exact delivered chart digest and invoke the installed renderer:

```sh
chart_ref='mirror.example.com/release/charts/web@sha256:<full-chart-digest>'
bale destinations inspect "$chart_ref" > destinations.json
"$(helm env HELM_PLUGINS)/bale/bale-relocate" \
  --chart="$chart_ref" --records-file=destinations.json \
  < rendered.yaml > relocated.yaml
```

Substitute the full chart digest and supply `rendered.yaml`. The filter cannot authenticate which chart produced an arbitrary stream: `--chart` is your assertion. It performs no registry access. `--records-file` accepts 4 MiB; alternative `--records-json` accepts 64 KiB. Add selectors when needed. Use this path to inspect diagnostics that Helm may hide.

## Reuse after transfer

A new capture can use the delivered chart and discovery plugin references. Supply delivered image references in discovery values using the fields that chart defines, and keep the `chart` blob binding. Bale generates a fresh declaration; historical recipes and attachments stay unchanged. See [plugin reuse](plugins.md#reuse-a-delivered-plugin).

## Generate a declaration manually

For previewing discovery or attaching a source declaration separately, follow the [manual request workflow](reference/helm-discovery.md#generate-a-declaration-manually).

## Offline build

The Nix package excludes Helm's cluster-client imports while retaining its renderer. Direct Go builds include the larger unmodified dependency graph. See the [offline build reference](reference/helm-discovery.md#offline-build) and [runtime contract](reference/plugin-runtime.md).
