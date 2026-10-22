# Plugin protocol and runtime

Implement a WASI Preview 1 command to produce a Bale [dependency declaration](../dependencies.md). For registry publication and recipe selection, use the [plugin guide](../plugins.md).

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

The [declaration schema](../dependencies.md) applies, including optional `context`, `select` and `follow` fields, explicit registry references and deduplication. Write diagnostics to stderr and exit nonzero on failure; other text on stdout makes the response invalid.

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

Save this as `request.json`:

```json
{"refs":["registry.example.com/images/web:stable"]}
```

Run it locally, or publish the module for recipe selection:

```sh
bale dependencies generate --plugin-file references.wasm --input request.json
bale plugins push -f references.wasm registry.example.com/plugins/references:1
```

The generated authoring YAML is:

```yaml
dependencies:
    - ref: registry.example.com/images/web:stable
```

Other languages can produce compatible WASI Preview 1 command modules using the same streams and JSON contract.

## Runtime and cache boundaries

Bale uses Wago behind an internal adapter; the [Go API](../sdk.md#run-a-dependency-plugin) and portable contract expose no engine-specific types. Wago is a prerelease dependency. Native caches are engine-private and rebuildable from Wasm.

Helm's [`extism/v1` runtime](https://github.com/helm/helm/blob/v4.3.0/internal/plugin/runtime_extismv1.go) uses a different entry point and host APIs. Bale's discovery modules are not interchangeable with Helm Extism plugins. The native Bale Helm consumer uses Helm's `postrenderer/v1` subprocess contract instead.

Cached code is native executable content. Keep the cache local and trusted; do not distribute it as a plugin or include it in an OCI bundle. Share the original `.wasm` module instead. The cache directory must belong to the current user with mode `0700`; Bale creates it with that mode if absent. Insecure or corrupt cache entries fail the run. The SDK disables persistent caching by default; callers opt in with `CacheDirectory`.

Disk caching requires supported Unix ownership and no-follow file operations. On other platforms, leave `CacheDirectory` empty or pass `--plugin-cache ''`.

Cache identity includes the host executable digest, source Wasm digest, platform and compiler settings. Rebuilding Bale or an SDK host produces a separate cache automatically; no runtime-version fields need to be maintained in plugin files.

Runs serialize within one Bale process to limit concurrent compiler and guest memory use. Waiting for that permit is cancellable. Compilation is synchronous and cannot currently be interrupted: cancellation is checked before and after compilation, so a cold compile can delay cancellation. The execution timeout applies to the guest, not compilation.

Guest linear-memory limits exclude the compiler, compiled code and host allocations. Measure large modules and apply host process limits when total memory must be bounded.
