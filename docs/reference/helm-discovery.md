# Helm discovery reference

The [Helm guide](../helm.md) covers capture and deployment. This reference describes the discovery request and the images it can find.

## Generate a declaration manually

Place the chart at `chart.tgz`, with every declared subchart packaged recursively under `charts/`, including disabled dependencies. Build a request with Python's standard library:

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

Supply YAML text in `valuesFiles`, in precedence order. Inline `values` override those files.

Review `dependencies.yaml`, then attach it to the matching OCI chart:

```sh
bale dependencies attach registry.example.com/charts/web:1.4 -f dependencies.yaml
```

The request archive must match the chart you attach to; Bale does not compare them automatically. To generate and attach together, replace `-f dependencies.yaml` with `--plugin registry.example.com/plugins/helm:1 --input request.json`.

Use `--plugin-file result-helm-plugin/lib/bale/plugins/helm.wasm` to run a local module during development.

Without a configured plugin, capture follows one matching source declaration automatically. Select among several profiles in the [recipe](../recipes.md#control-dependency-following). [Destination records](../destinations.md) report the captured image placements; chart bytes stay unchanged.

## Request fields

Requests are strict UTF-8 JSON. Unknown, duplicate and null top-level fields are rejected.

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

Helm applies chart defaults, ordered values files, then inline values. Its SDK processes dependency aliases, tags, conditions and imported values, validates schemas, and checks the chart's `kubeVersion` constraint.

Rendering is offline: `lookup` returns empty results and DNS is disabled. Values schemas must be self-contained. Charts needing live cluster state may require explicit values or another preparation step.

## Image inventory

The plugin renders parent charts, subcharts and lifecycle/test hooks. It inspects pod specifications without executing hooks or installing resources.

| API version | Supported kinds |
| --- | --- |
| `v1` | `Pod`, `PodTemplate`, `ReplicationController`, `List` |
| `apps/v1` | `Deployment`, `StatefulSet`, `DaemonSet`, `ReplicaSet` |
| `batch/v1` | `Job`, `CronJob` |

It reads `containers`, `initContainers` and `ephemeralContainers`, recursively inspecting `List` items with the same supported kinds and versions.

Malformed supported workloads and invalid image references fail generation without a partial declaration.

Custom resources, CRDs, ConfigMaps, annotations and arbitrary `image` fields are not scanned. Use `additionalImages` or an explicit declaration for them. Resources created by executing hooks or operators are outside the rendered inventory.

References are normalized, sorted and deduplicated: `busybox` becomes `docker.io/library/busybox:latest`. Tags are preserved unless a digest is present; tag-plus-digest becomes digest-only. Output is Bale declaration JSON with the selected context; the CLI converts it to authoring YAML.

## Limits and Go API

The request, including base64 chart bytes and values, must fit within 8 MiB; an 8 MiB archive will not fit after encoding. Declaration output is limited to 4 MiB and the expanded chart to Helm's stock 100 MiB. See [plugin runtime limits](plugin-runtime.md) for memory, timeout and caching.

Recipe bindings verify the selected chart layer. Manual requests supply local file contents as JSON.

The Go module `github.com/strio-dev/bale/plugins/helm` exposes `helmplugin.Discover(ctx, inputBytes)`. Native calls have host access, including external schema references; use Bale's runner for sandboxing. This request schema is specific to Helm discovery.

## Offline build

Nix applies [`helm-offline.patch`](https://github.com/strio-dev/bale/blob/main/nix/patches/helm-offline.patch) and selects `helm_offline`. This separates cluster-client imports from the renderer while retaining Helm's loader, templates, Sprig, dependency processing and schema validation. Direct Go builds use unmodified Helm and produce a larger module.

The vendor hash verifies the patched tree. Review the patch and default API-version snapshot when upgrading Helm or Kubernetes; see [patch maintenance](https://github.com/strio-dev/bale/blob/main/nix/patches/README.md).
