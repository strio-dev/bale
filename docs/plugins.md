# Dependency plugins

Plugins discover application dependencies and return a [dependency declaration](dependencies.md). Select a registry-hosted plugin in the recipe to run it automatically during bundle creation. Bale captures its portable Wasm graph, resolved digest and generated declaration alongside the selected artifact and dependencies.

Dependency plugins use WASI Preview 1 and JSON. They run without a container runtime, native Helm, Rust host helper or CGO. The [Helm discovery plugin](helm.md) finds images from an offline chart render. A separate [native Helm post-renderer](helm.md#use-delivered-images-in-helm) applies delivered image references when Helm renders or deploys the chart.

## Configure discovery in a recipe

```yaml
include:
  - ref: registry.example.com/charts/web:1.4
    plugin:
      ref: registry.example.com/plugins/helm:1
      input:
        schemaVersion: 1
        release: web
        namespace: production
      blobs:
        chart:
          mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
```

`plugin.ref` requires an explicit registry and tag or digest. `input` is a plugin-defined JSON object authored in YAML; it defaults to `{}`. Supply values directly, rather than host file paths. Nested nulls, booleans and numbers retain their JSON meanings.

The Helm example discovers chart defaults. Images introduced by custom deployment values belong in ordinary recipe `include` entries; after transfer, set those deployment values to their destination references. Bale does not need the deployment values file. Optional plugin render inputs remain available when a chart needs them; see the [Helm guide](helm.md).

`blobs` optionally binds top-level request fields to layers of the selected artifact by media type. Each binding must match exactly one layer; Bale verifies its bytes and inserts their base64 encoding into that request field. Bound fields must not also appear in literal `input`. The chart archive above therefore comes from the selected OCI chart, including when its tag changes.

```sh
bale create -f bale.yaml registry.example.com/bundles/release:v1
```

Creation runs the plugin, validates its declaration and resolves the declared dependencies. It stores the generated declaration in the bundle without publishing an attachment to the source registry. The captured recipe retains literal input and blob bindings; it does not embed injected base64 payloads. A dry run also executes discovery, but publishes nothing.

Creation reports plugin fetching and dependency discovery on stderr, identifying the source input and selected plugin. Failures retain the input index, subject and plugin reference alongside the original diagnostics. `--quiet` suppresses progress. `create --dry-run` shows the resolved inventory by default; explicit JSON and template formats remain available. See [creation output](transfers.md#output-and-errors).

Each input selects one plugin. Do not combine it with `dependencies.follow: none` or `dependencies.select`. Separate recipe inputs can use different profiles of the same artifact; their generated declarations are selected independently.

Locked pull, inspection and push use captured content without running plugins. Latest pull refreshes the recipe and requires explicit execution authorization:

```sh
bale pull registry.example.com/bundles/release:v1 ./refreshed --mode latest --allow-plugins
```

## Publish a plugin

Publish a portable WASI command module to an OCI repository:

```sh
bale plugins push -f references.wasm registry.example.com/plugins/references:1
```

The OCI image uses an `application/vnd.wasm.config.v0+json` config declaring `architecture: wasm`, `os: wasip1` and its ordered layer digests, plus one `application/wasm` layer. An OCI index is accepted when it selects exactly one `wasm/wasip1` manifest. Captured bundles retain the selected plugin's root graph, including the index when present. Native compiled caches remain local.

Registry commands use Docker credentials and the normal exact-host `--plain-http HOST` scopes. Pin a plugin manifest or index digest in the recipe when the version must remain fixed across fresh captures.

## Reuse a delivered plugin

After delivery, select the plugin's destination repository and restored tag, or its unchanged digest, in a new recipe. It can run with an empty native cache using only the delivered portable Wasm. The original registry and local bundle layout are no longer needed.

Use destination references for the new recipe's artifacts and any references supplied in plugin input. Delivery preserves captured recipes and historical declarations unchanged. Configured discovery generates and selects a fresh declaration for the new capture; previous source declarations can remain attached without being followed. See [Helm reuse](helm.md#reuse-after-transfer) for chart-specific inputs.

## Generate or attach a declaration manually

For a preview or a separately published source attachment, supply an OCI plugin reference and a JSON request file:

```sh
bale dependencies generate --plugin registry.example.com/plugins/references:1 --input request.json > dependencies.yaml
bale dependencies attach registry.example.com/charts/web:1.4 -f dependencies.yaml
```

Generate fetches the plugin, validates its response and writes authoring YAML, omitting the JSON `schemaVersion` field. Attach can generate and publish a declaration in one command:

```sh
bale dependencies attach registry.example.com/charts/web:1.4 --plugin registry.example.com/plugins/references:1 --input request.json
```

For local development, use `--plugin-file references.wasm` instead of `--plugin OCIREF`. Both require `--input`; select one plugin source. Manual requests must already contain any required archive bytes. Dependency references remain unresolved until bundle creation.

Create, authorized latest pull and the manual plugin commands accept these host runtime settings:

| Option | Default | Meaning |
| --- | --- | --- |
| `--plugin-cache DIR` | User cache directory, under `bale/plugins` | Trusted local compiled-code cache. An explicit empty value disables it. |
| `--plugin-memory BYTES` | `268435456` (256 MiB) | Maximum guest linear memory; a multiple of 65,536 bytes, at most 4 GiB. |
| `--plugin-timeout DURATION` | `30s` | Guest execution timeout. |

For a run without a persistent cache, use `--plugin-cache ''`. Runtime policy belongs to the host, rather than the portable recipe.

## Module contract

The guest exports the standard WASI command entry point, `_start`. It receives one plugin-defined UTF-8 JSON request on stdin. Its only argument is `bale-plugin`; Bale does not pass host environment variables, mount host filesystems, or provide network access. Supply values and application inputs using a schema defined by that plugin. Recipe blob bindings can supply archive bytes from the selected artifact.

On success, stdout must contain one UTF-8 JSON dependency declaration with `schemaVersion: 1`:

```json
{
  "schemaVersion": 1,
  "dependencies": [
    {"ref": "registry.example.com/images/web:stable"}
  ]
}
```

The existing declaration schema applies, including optional `context`, `select`, and `follow` fields, explicit registry references, and deduplication. There is no plugin-specific output envelope. Write diagnostics to stderr and exit nonzero on failure; logs or other text on stdout make the response invalid.

Input is limited to 8 MiB, stdout to 4 MiB, stderr to 64 KiB, and the Wasm module to 128 MiB. Exceeding an output limit fails the run even if the guest ignores the write error. Invalid output and failed runs do not produce a declaration.

## Build a small Go plugin

Save this example as `references.go`. It converts a request containing `refs` into declaration JSON; application integrations replace that discovery step with their own logic.

```go
package main

import (
    "encoding/json"
    "fmt"
    "os"
)

func main() {
    var request struct {
        Refs []string `json:"refs"`
    }
    if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
    dependencies := make([]map[string]string, 0, len(request.Refs))
    for _, ref := range request.Refs {
        dependencies = append(dependencies, map[string]string{"ref": ref})
    }
    response := struct {
        SchemaVersion int                 `json:"schemaVersion"`
        Dependencies  []map[string]string `json:"dependencies"`
    }{1, dependencies}
    if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
}
```

Compile it with Go's standard WASI target:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -o references.wasm references.go
```

Publish the module with the push command above. Save this as `request.json`, then run generate:

```json
{"refs":["registry.example.com/images/web:stable"]}
```

The resulting authoring YAML is:

```yaml
dependencies:
    - ref: registry.example.com/images/web:stable
```

The contract uses standard WASI streams and JSON. Plugins can use other languages that produce compatible WASI Preview 1 command modules; they do not need the Bale Go SDK or a Wago guest SDK.

## Runtime and cache boundaries

Bale currently uses Wago behind an internal adapter. The [Go API](sdk.md#run-a-dependency-plugin), CLI, and portable module contract expose no engine-specific types. Replacing the engine should preserve those interfaces; compiled caches are private to the engine and can be rebuilt from the original Wasm module.

Helm 4 uses a different plugin profile: its `extism/v1` runtime uses the pure-Go Extism SDK and Wazero. It does not require a Rust helper or CGO in the Go host. Its entry point and host APIs differ from Bale's WASI command contract; sharing Wasm or OCI storage does not make the modules interchangeable. See [Helm's runtime implementation](https://github.com/helm/helm/blob/v4.3.0/internal/plugin/runtime_extismv1.go) and [Extism's Go host](https://github.com/extism/go-sdk/blob/v1.7.1/plugin.go).

Bale's Helm consumer uses Helm's native `postrenderer/v1` subprocess contract. One platform-specific plugin package contains the relocation executable; Helm invokes it directly. It uses the same destination records independently of Bale's discovery runner.

Wago remains an intentional runtime and ABI exception to Helm alignment. After separating Helm's cluster-client imports from the [offline discovery build](helm.md#offline-build), the module is approximately 22.4 MB. Standalone hosts running the same discovery implementation measured:

| Host | Cold wall time / peak RSS | Cached wall time / peak RSS |
| --- | --- | --- |
| Extism Go 1.7.1 / Wazero 1.12.0 | 4.685 s / 374.0 MiB | 0.333 s / 184.7 MiB |
| Wago | 1.025 s / 204.5 MiB | 0.083 s / 89.2 MiB |

These are one cold and one cached fresh-process run per runtime on one Linux AMD64 machine with four CPUs and default Go garbage collection. Cold starts with an empty compilation cache. The hosts use their respective guest entry points; these are custom standalone hosts, not the Helm CLI or the Bale CLI. The earlier module, before the import-boundary patch, was 81.3 MB and used substantially more compiler memory. This workload-specific comparison supports retaining Wago; it does not establish a general runtime ranking or a process-memory bound. Helm deployment plugins should use Helm's supported package and runtime contracts.

Cached code is native executable content. Keep the cache local and trusted; do not distribute it as a plugin or include it in an OCI bundle. Share the original `.wasm` module instead. The cache directory must belong to the current user with mode `0700`; Bale creates it with that mode if absent. Insecure or corrupt cache entries fail the run. The SDK disables persistent caching by default; callers opt in with `CacheDirectory`.

Disk caching requires supported Unix ownership and no-follow file operations. On other platforms, leave `CacheDirectory` empty or pass `--plugin-cache ''`.

Cache identity includes the host executable digest, source Wasm digest, platform and compiler settings. Rebuilding Bale or an SDK host produces a separate cache automatically; no runtime-version fields need to be maintained in plugin files.

Runs serialize within one Bale process to limit concurrent compiler and guest memory use. Waiting for that permit is cancellable. Compilation is synchronous and cannot currently be interrupted: cancellation is checked before and after compilation, so a cold compile can delay cancellation. The execution timeout applies to the guest, not compilation.

The linear-memory limit does not cap total process memory: the compiler, compiled code, and host also allocate memory. Wago is a prerelease dependency. Engine replacement and cache isolation keep that choice out of the plugin wire format; they do not remove the need to measure large modules and apply process limits where required.
