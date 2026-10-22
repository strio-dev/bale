# Helm discovery reference

The [Helm guide](../helm.md) covers capture and deployment. This reference describes the discovery request and the images it can find.

## Generate a declaration manually

Place your packaged chart at `chart.tgz`. All declared chart dependencies must already be bundled recursively under `charts/`, including dependencies disabled by this profile. Bale does not fetch them. Build a request from the local files with Python's standard library:

```sh
python3 - <<'PY'
import base64
import json
from pathlib import Path

request = {
    "schemaVersion": 1,
    "chart": base64.b64encode(Path("chart.tgz").read_bytes()).decode("ascii"),
    "valuesFiles": [],
    "values": {},
    "release": "web",
    "namespace": "production",
    "context": "production",
}
Path("request.json").write_text(json.dumps(request) + "\n", encoding="utf-8")
PY
bale dependencies generate --plugin registry.example.com/plugins/helm:1 --input request.json > dependencies.yaml
```

To supply render values, add YAML texts to `valuesFiles` in precedence order. Add inline overrides to the `values` object; they take precedence over the files.

Review `dependencies.yaml`, then attach it to the matching OCI chart:

```sh
bale dependencies attach registry.example.com/charts/web:1.4 -f dependencies.yaml
```

The archive in the request must match the chart you attach to. Bale does not compare those bytes automatically. To generate and attach directly, replace `-f dependencies.yaml` with `--plugin registry.example.com/plugins/helm:1 --input request.json`.

Use `--plugin-file result-helm-plugin/lib/bale/plugins/helm.wasm` to run a local module during development.

Without a configured plugin, bundle creation follows a single matching source declaration automatically. If the chart has several profiles attached, select the intended declaration context in the [bundle recipe](../recipes.md#control-dependency-following). The chart archive remains unchanged; its [destination records](../destinations.md) report the captured image placements.

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

## Limits and Go API

The complete request, including the base64 chart and values, must fit within 8 MiB. Base64 encoding increases the archive's size, so an 8 MiB archive will not fit. Declaration output is limited to 4 MiB, and Helm's loader retains its stock 100 MiB expanded-chart limit. Guest memory, execution timeout, compiler memory, and local caching follow the [plugin runtime limits](plugin-runtime.md).

Recipe blob bindings read and verify the selected chart layer. Manual request preparation reads local files and passes their contents to the sandbox as JSON.

The independent Go module `github.com/strio-dev/bale/plugins/helm` exposes `helmplugin.Discover(ctx, inputBytes)`, returning declaration JSON or an error. Native calls have host access, including external schema references; use Bale's plugin runner for sandboxing. This plugin-specific request schema is separate from the generic WASI protocol.

## Offline build

The Nix build applies [`helm-offline.patch`](https://github.com/strio-dev/bale/blob/main/nix/patches/helm-offline.patch) to vendored Helm dependencies and selects `helm_offline`. It separates cluster-client imports from rendering and retains Helm's chart loader, templates, Sprig functions, dependency processing and schema validation. Direct Go builds use unmodified Helm and produce a larger module.

The fixed vendor hash verifies the patched tree. Helm or Kubernetes upgrades require reviewing the patch and its default API-version snapshot; see the [patch maintenance notes](https://github.com/strio-dev/bale/blob/main/nix/patches/README.md). Runtime and memory limits are documented in the [plugin runtime reference](plugin-runtime.md).
