# Helm dependency discovery

A standard WASI command that renders a packaged Helm chart offline and emits a
Bale dependency declaration. The plugin uses Helm 4.3; it needs neither a Helm
executable nor a container runtime.

Build the offline Wasm package from the repository root:

```sh
nix build --no-update-lock-file path:.#helm-plugin
```

The module is `result/lib/bale/plugins/helm.wasm`. Nix applies a checked-in patch
to its vendored Helm dependency, verifies the vendor hash and builds with
`helm_offline` to exclude Kubernetes cluster clients. It uses the standard Go
builder without a fork, Python or changes to the Go module cache. See the
[patch notes](../../nix/patches/README.md) for scope and upgrade checks.

For development, build and test from this directory:

```sh
CGO_ENABLED=0 GOOS=wasip1 GOARCH=wasm go build -trimpath -o helm.wasm ./cmd/bale-helm
go test ./...
```

These direct Go commands use unmodified Helm. The resulting Wasm includes the
larger client dependency graph; `helm_offline` is available only in the patched
Nix build. Nix also runs native checks without the offline tag, preserving Helm's
client APIs in that build.

Send one UTF-8 JSON request on standard input, with the packaged chart encoded as
base64. Successful standard output is the bare declaration:

```json
{
  "schemaVersion": 1,
  "chart": "<base64 chart.tgz>",
  "release": "example",
  "namespace": "default",
  "valuesFiles": ["image:\n  tag: stable\n"],
  "values": {"image": {"tag": "1.2.3"}},
  "capabilities": {
    "kubeVersion": "1.36.0",
    "apiVersions": ["example.org/v1"]
  },
  "isUpgrade": false,
  "context": "helm-render",
  "additionalImages": ["ghcr.io/example/operator:v1"]
}
```

```json
{"schemaVersion":1,"context":"helm-render","dependencies":[{"ref":"ghcr.io/example/operator:v1"}]}
```

`schemaVersion`, `chart`, a valid Helm release name and a nonblank namespace are
required. All other fields are optional. Chart defaults are followed by
`valuesFiles` YAML texts in order, then inline `values`; each YAML text may contain
multiple documents. The default profile is install, revision 1, with Helm's stock
Kubernetes capabilities. `isUpgrade` selects upgrade; `apiVersions` adds to stock
versions. `kubeVersion` overrides the default and must satisfy the chart's declared
constraint. `context` defaults to `helm-render`. Null typed request fields,
unknown request fields, duplicate JSON keys, trailing JSON and invalid UTF-8 are
rejected. Nulls inside inline values retain Helm's value deletion semantics.

All declared chart dependencies must already exist inside the archive, including
nested, aliased and disabled dependencies, with compatible versions. Helm handles
conditions, tags, import-values, template functions and values schema validation.
The plugin does not download chart dependencies, connect to a cluster, enable
Helm DNS lookups, install workloads or execute hooks. Run the Wasm command in
Bale's sandbox to deny host networking and filesystem access; the Go API by
itself is not a sandbox for templates and externally referenced schemas. Helm's
archive loader enforces its stock 100 MiB expanded archive limit; request JSON is
limited to 8 MiB and output JSON to 4 MiB.

Discovery includes rendered Pod, PodTemplate and ReplicationController resources
in core `v1`; Deployment, StatefulSet, DaemonSet and ReplicaSet in `apps/v1`; Job
and CronJob in `batch/v1`; and nested core `v1` Lists. Images come only from their
PodSpec containers, init containers and ephemeral containers, including rendered
Helm hooks. Empty documents and `NOTES.txt` are skipped. Image names are normalized
with Docker Hub and `latest` defaults; digest references discard an accompanying
tag. Results are sorted and deduplicated.

Custom resources, operator conventions, CRD files and arbitrary image-like text
are not interpreted. Declare their required images through `additionalImages`.
One request covers one rendering profile: use every applicable profile to capture
conditional images. The declaration identifies dependencies to bundle; it does
not rewrite a chart or provide Helm/Flux consumption integration.

The Go API is `helmplugin.Discover(context.Context, []byte) ([]byte, error)`. It
returns no declaration on failure. The standalone command writes diagnostics to
standard error and exits nonzero; it writes no successful JSON on those failures.
