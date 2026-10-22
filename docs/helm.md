# Helm discovery and relocation

Bale's Helm plugin renders a packaged chart offline and returns a [dependency declaration](dependencies.md) containing its image references. Select the registry plugin in a recipe so bundle creation captures those images automatically. Discovery uses Helm 4.3's Go renderer inside a portable WASI module; it needs no native Helm executable or container runtime.

Normal discovery captures the chart's default image dependencies. If deployment values introduce another image, add that image as an ordinary `include` entry in the recipe. After transfer, write deployment values pointing it at its destination reference; Bale does not generate that values file or check that every override was captured. The [Helm offline example](examples/helm-offline/README.md) shows this workflow.

Discovery describes one render profile: the chart, optional render inputs, release, namespace, and capabilities supplied in the request. It does not inventory every possible values combination. After delivery, the separate [native Helm integration](#use-delivered-images-in-helm) reads destination attachments and applies image mappings during deployment. [Flux consumption](flux.md) uses Flux Operator and ordinary Flux resources.

Bale hosts the discovery Wasm during capture; the native integration invokes Helm during deployment. The [runtime comparison](plugins.md#runtime-and-cache-boundaries) explains the discovery execution profile.

For the complete capture, offline transfer and deployment workflow, use the [Helm offline example](examples/helm-offline/README.md). It carries both plugins, discovers the chart's images during creation and installs the native integration from the destination registry.

## Publish and configure discovery

From the Bale checkout, enter the development shell with `nix develop`, then build and publish the module:

```sh
nix build --no-update-lock-file path:.#helm-plugin --out-link result-helm-plugin
bale plugins push -f result-helm-plugin/lib/bale/plugins/helm.wasm registry.example.com/plugins/helm:1
```

`just helm-plugin-build` runs the same build. Select the published plugin in `bale.yaml`:

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

```sh
bale create -f bale.yaml registry.example.com/bundles/release:v1
```

Bale takes the packaged chart bytes from the selected OCI chart layer, verifies them and passes them as the base64 `chart` field. All chart dependencies must already be bundled recursively under `charts/`, including dependencies disabled by this profile. Discovery does not fetch them.

Creation captures the plugin's resolved digest and graph, its generated declaration, and the discovered images. It does not publish a declaration to the chart's source registry. Separate inputs can render different profiles of the same chart. Locked transfer never reruns discovery; refreshing through latest pull requires `--allow-plugins`.

Deployment values stay separate from this recipe. The optional discovery `values` and `valuesFiles` fields remain available for charts that require render inputs; copying deployment overrides into them is not required.

The chart archive remains unchanged, and delivery exposes captured image destinations through [destination records](destinations.md). Use the post-renderer below to apply those destinations, or set them in the values fields that the chart defines.

## Offline build

The Nix package applies the checked-in `nix/patches/helm-offline.patch` to its vendored Helm dependencies and selects the `helm_offline` build tag. The patch separates cluster-client imports from rendering and captures the pinned dependencies' default API versions. Helm's chart loader, template engine, Sprig functions, dependency processing and schema validation remain in use. The resulting module is 22.4 MB, compared with 81.3 MB before the patch.

The fixed vendor hash verifies the patched dependency tree. Helm or Kubernetes upgrades require reviewing the patch and refreshing its capability snapshot; maintenance instructions are in `nix/patches/README.md`. Default native builds retain the cluster-aware APIs. The public Go module still selects unmodified Helm, so direct Go builds produce the larger module.

With the Nix-built module, the Bale CLI rendered Podinfo 6.9.4 in a median 1.062 seconds with 210.6 MiB peak process RSS from an empty compilation cache, and 0.111 seconds with 96.8 MiB from a populated cache. Each median covers three fresh-process runs on one Linux AMD64 machine with four CPUs and default Go garbage collection. These measurements are examples, not memory or latency guarantees; the 256 MiB guest limit does not cap total process memory.

## Reuse after transfer

To capture another bundle using a delivered plugin, create a recipe with the delivered chart and plugin references. Set the chart's image values to its delivered image references as well, using the fields that chart defines. Keep the `chart` blob binding so discovery reads the delivered archive.

The delivered plugin runs from its portable Wasm, including with an empty native cache and the original registry offline. It produces a new declaration for the supplied values. Existing attachments retain their historical selectors; the configured plugin's new declaration is selected for this capture without modifying those attachments or the chart.

## Generate a declaration manually

Place your packaged chart at `chart.tgz` and a values file at `values.yaml`. All declared chart dependencies must already be bundled recursively under `charts/`, including dependencies disabled by this profile. Bale does not fetch them. Build a request from the local files with Python's standard library:

```sh
python3 - <<'PY'
import base64
import json
from pathlib import Path

request = {
    "schemaVersion": 1,
    "chart": base64.b64encode(Path("chart.tgz").read_bytes()).decode("ascii"),
    "valuesFiles": [Path("values.yaml").read_text(encoding="utf-8")],
    "values": {},
    "release": "web",
    "namespace": "production",
    "context": "production",
}
Path("request.json").write_text(json.dumps(request) + "\n", encoding="utf-8")
PY
bale dependencies generate --plugin registry.example.com/plugins/helm:1 --input request.json > dependencies.yaml
```

Use an empty `valuesFiles` array when no values files are needed, or add more YAML texts in precedence order. Add inline overrides to the `values` object; they take precedence over the files.

Review `dependencies.yaml`, then attach it to the matching OCI chart:

```sh
bale dependencies attach registry.example.com/charts/web:1.4 -f dependencies.yaml
```

The archive in the request must match the chart you attach to. Bale does not compare those bytes automatically. To generate and attach directly, replace `-f dependencies.yaml` with `--plugin registry.example.com/plugins/helm:1 --input request.json`.

Use `--plugin-file result-helm-plugin/lib/bale/plugins/helm.wasm` to run a local module during development.

Without a configured plugin, bundle creation follows a single matching source declaration automatically. If the chart has several profiles attached, select the intended declaration context in the [bundle recipe](recipes.md#control-dependency-following). The chart archive remains unchanged; its [destination records](destinations.md) report the captured image placements.

## Request fields

Input is strict UTF-8 JSON. Unknown and duplicate fields are rejected. Required fields must be present, and top-level fields must not be `null`.

| Field | Meaning |
| --- | --- |
| `schemaVersion` | Required; `1`. |
| `chart` | Required; base64-encoded packaged chart `.tgz` bytes. |
| `release` | Required; a valid Helm release name. |
| `namespace` | Required; a nonblank namespace. |
| `valuesFiles` | Optional ordered array of raw YAML strings, not file paths. Later files override earlier files. |
| `values` | Optional inline values object, applied after `valuesFiles`. |
| `context` | Declaration label; defaults to `helm-render`. Use separate labels for separate profiles. |
| `isUpgrade` | Defaults to `false` for an install render; `true` selects upgrade rendering. |
| `capabilities.kubeVersion` | Optional Kubernetes version; defaults to the stock Helm 4.3 SDK capability value. |
| `capabilities.apiVersions` | Optional array added to Helm's default API versions. |
| `additionalImages` | Optional image reference strings to include alongside discovered images. |

Helm chart defaults apply first, followed by ordered values files and inline values. The stock SDK processes dependency aliases, tags, conditions, and imported values, validates values schemas, and checks the chart's `kubeVersion` constraint against the requested capabilities.

Rendering is offline. `lookup` has no cluster client and returns empty results; DNS lookups are disabled. Values schemas must be self-contained: external references that require fetching cannot be resolved in the sandbox. A chart that depends on live cluster state may therefore need explicit values or a different preparation step.

## Image inventory

The plugin renders parent and subchart templates, including lifecycle and test hooks. It inspects their pod specifications without executing hooks or installing any resources.

| API version | Supported kinds |
| --- | --- |
| `v1` | `Pod`, `PodTemplate`, `ReplicationController`, `List` |
| `apps/v1` | `Deployment`, `StatefulSet`, `DaemonSet`, `ReplicaSet` |
| `batch/v1` | `Job`, `CronJob` |

For those pod specifications it reads `containers`, `initContainers`, and `ephemeralContainers`. `List` items are inspected recursively using the same supported kinds and versions.

Malformed supported workloads and invalid image references fail generation without a partial declaration.

Custom resources, CRDs, ConfigMaps, annotations, and arbitrary fields named `image` are not scanned heuristically. Include their image dependencies through `additionalImages` or another explicit declaration. Manifests generated only during hook execution or by an operator are outside this render's inventory.

Discovered and additional image references are normalized, sorted, and deduplicated. For example, `busybox` becomes `docker.io/library/busybox:latest`. Explicit tags are preserved unless a digest is present; a tag-plus-digest reference becomes a digest-only reference. The output is the existing Bale declaration JSON with the chosen context, without a Helm-specific envelope. The CLI converts it into authoring YAML.

## Limits and integration boundary

The complete request, including the base64 chart and values, must fit within 8 MiB. Base64 encoding increases the archive's size, so an 8 MiB archive will not fit. Declaration output is limited to 4 MiB, and Helm's loader retains its stock 100 MiB expanded-chart limit. Guest memory, execution timeout, compiler memory, and local caching follow the [plugin runtime limits](plugins.md#runtime-and-cache-boundaries).

The WASI sandbox grants no host filesystem, environment, or network access. Recipe blob bindings read and verify the selected chart layer. The optional Python preparation step reads local files and passes their contents in a manual JSON request. The module contains the Go Helm renderer and needs no CGO, Rust helper, or installed Helm command.

Go applications can use the independent module `github.com/strio-dev/bale/plugins/helm`, whose package name is `helmplugin`. `Discover(ctx, inputBytes)` returns declaration JSON bytes or an error. Calling it natively does not provide the WASI sandbox; stock Helm schema handling can access external references through the host. Use Bale's plugin runner for sandboxed execution. This application-specific module does not make Bale core parse Helm payloads, and its request format is separate from the generic plugin protocol and upstream Helm proposals.

## Use delivered images in Helm

The consumer is a native Helm 4 CLI plugin named `bale`. It reads the delivered chart's destination attachments, resolves the chart to a digest, and invokes Helm with its private post-renderer. Build and install it for the current platform:

```sh
nix build --no-update-lock-file path:.#helm-consumer --out-link result-helm-consumer
helm plugin install result-helm-consumer/share/helm/plugins/bale
```

The flake provides Linux AMD64 and ARM64 builds. Each package declares its OS and architecture; choose a matching package. One installation includes the CLI adapter and its private renderer. This native consumer runs separately from Bale's discovery Wasm and compilation cache.

### Package and transfer

Package the directory with Helm and publish it using its [standard OCI plugin format](https://github.com/helm/helm/blob/v4.3.0/pkg/registry/plugin.go). This example publishes a Linux AMD64 development build:

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

Add that OCI reference as an ordinary `include` entry in the Bale recipe alongside the chart. Preserve the repository's final name, `bale`, in its delivery path: Helm uses it to locate the archive layer. Use explicit platform tags; Helm does not select a native plugin from a multiarchitecture OCI index. After transfer, install the unsigned development package from its delivered reference:

```sh
helm plugin install oci://mirror.example.com/release/plugins/bale:0.0.0-linux-amd64 --verify=false
```

### Render or deploy

Use the delivered chart's explicit tag or digest:

```sh
helm bale template web oci://mirror.example.com/release/charts/web:1.4 \
  --namespace production -f production.yaml
helm bale upgrade web oci://mirror.example.com/release/charts/web:1.4 \
  --install --namespace production -f production.yaml --wait
```

The grammar is `helm bale [Bale selectors] install|upgrade|template RELEASE OCI_CHART [Helm flags]`. Put Helm command flags after the chart; global Helm flags such as `--namespace` retain their normal effect. An untagged chart requires an exact `--version`; implicit highest-version selection and version ranges are unsupported. Supply deployment values normally. For images introduced by those values, explicitly include the source images in the bundle and use their destination references in the deployment values.

The adapter resolves the chart tag once, validates its attachments, then passes the pinned chart to Helm. Registry lookup uses Helm's registry configuration with Docker credentials as a fallback. `--plain-http`, `--ca-file`, `--cert-file`, `--key-file`, `--username`, `--password` and `--insecure-skip-tls-verify` apply to lookup and chart retrieval. Prefer credential stores to passwords in arguments.

The selected metadata travels through a temporary file with mode `0600`, removed when Helm exits. Inspection permits at most 256 advertised referrers and 4 MiB of inventory and attachment reads, including validation rereads; serialized records have a separate 4 MiB limit. Helm retains its flags, environment, input/output streams and exit status. An existing `--post-renderer` cannot be combined with this integration. Rollback, history, uninstall and other actions use ordinary Helm commands; stored release manifests already contain the pinned image references.

### Private registry access

Use Helm's [registry login](https://helm.sh/docs/helm/helm_registry_login/) to populate its default credential location. This example reads the registry password from a locally provisioned file:

```sh
export HELM_CONFIG_HOME="$PWD/helm-config"
export HELM_REGISTRY_CONFIG="$HELM_CONFIG_HOME/registry/config.json"
helm registry login mirror.example.com --username deploy \
  --password-stdin --ca-file registry-ca.crt < registry-password
helm plugin install oci://mirror.example.com/release/plugins/bale:0.0.0-linux-amd64 \
  --verify=false --ca-file registry-ca.crt
helm bale upgrade web oci://mirror.example.com/release/charts/web:1.4 \
  --install --namespace production --values production.yaml --wait \
  --ca-file registry-ca.crt
```

Login stores credentials, not CA trust: pass `--ca-file` for subsequent operations. The adapter applies those options to attachment lookup and Helm's chart retrieval. Workload image pulls use separate Kubernetes credentials and node trust; see the [private registry configuration](flux.md#private-registry-configuration).

Helm 4.3.0's OCI plugin installer reads the default Helm credential location or Docker credentials; a custom `--registry-config` alone does not reach that installer. Keeping `HELM_CONFIG_HOME` and `HELM_REGISTRY_CONFIG` aligned as above supports login, plugin installation and chart retrieval. See the [pinned registry client implementation](https://github.com/helm/helm/blob/bec5b06ed841fe5269972d864d5177944fd5970f/pkg/registry/client.go#L87-L119).

### Selection and validation

The consumer requires one matching local physical attachment and one selected dependency resolution. It never picks the newest attachment or unions captures. If selection is ambiguous, add the needed selectors before the action:

```sh
helm bale --context production --bundle 'mirror.example.com/release@sha256:<full-bundle-digest>' \
  upgrade web oci://mirror.example.com/release/charts/web:1.4 \
  --install --namespace production -f production.yaml
```

| Selector | Value |
| --- | --- |
| `--bundle` | Delivered bundle repository and digest. |
| `--source` | Original chart repository and digest. |
| `--capture` | Owning capture digest. |
| `--declaration` | Selected declaration digest. |
| `--context` | Declaration context string. |

A selected declaration with no dependencies is valid for a chart without images. Missing, disabled or unselected dependency observations fail before Helm starts.

For the [supported workload kinds](#image-inventory), the renderer replaces normalized original image selectors or captured source references with pinned destination references. It includes hooks, init containers, ephemeral containers and nested `List` items. Destination references and valid unmapped image references remain unchanged. A custom image already set to its transferred location needs no extra attachment or override handling. Malformed supported workloads, invalid image references and invalid selected metadata still fail without partial YAML output. Helm can hide renderer diagnostics; the offline filter below can report them directly.

Custom resources, CRDs, ConfigMaps and other unknown fields retain their content; the filter does not infer image locations in them. Capturing these references with `additionalImages` does not make them rewritable: they need a consumer that understands their schema. YAML anchors, aliases and merge keys on paths requiring mutation are rejected. Input is limited to 8 MiB of rendered YAML and buffered output to 16 MiB. The chart archive remains unchanged.

### Offline filter

The installed executable can also filter an independently rendered stream:

```sh
chart_ref='mirror.example.com/release/charts/web@sha256:<full-chart-digest>'
bale destinations inspect "$chart_ref" > destinations.json
"$(helm env HELM_PLUGINS)/bale/bale-relocate" \
  --chart="$chart_ref" --records-file=destinations.json \
  < rendered.yaml > relocated.yaml
```

Here `--chart` is a caller assertion: the filter cannot authenticate which chart produced an arbitrary YAML stream. Add selectors directly to the filter as needed. `--records-file` accepts at most 4 MiB; the alternative `--records-json` argument accepts 64 KiB. The filter performs no registry access.
