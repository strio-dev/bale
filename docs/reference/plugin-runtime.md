# Plugin protocol and runtime

Implement a WASI Preview 1 command to produce a Bale [dependency declaration](../dependencies.md). For registry publication and recipe selection, use the [plugin guide](../plugins.md).

## Module contract

The guest exports the standard WASI command entry point, `_start`. It receives one plugin-defined UTF-8 JSON request on stdin. Its only argument is `bale-plugin`; Bale does not pass host environment variables, mount host filesystems, or provide network access. Supply values and application inputs using the plugin’s schema. Recipe inputs preserve JSON types; blob bindings can supply archive bytes from the selected artifact. Captures retain literal inputs and binding definitions, not the injected archive bytes.

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

From a Bale checkout, compile it with the pinned Go toolchain's standard WASI target:

```sh
mkdir -p references
nix develop --no-update-lock-file path:.#runtime --command \
  env GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -o references/plugin.wasm references.go
```

Save this as `request.json`:

```json
{"refs":["registry.example.com/images/web:stable"]}
```

Run the module locally:

```sh
bale dependencies generate --plugin-file references/plugin.wasm --input request.json
```

The generated authoring YAML is:

```yaml
dependencies:
    - ref: registry.example.com/images/web:stable
```

Other languages can produce compatible WASI Preview 1 command modules using the same streams and JSON contract.

## Package for publication

Save this as `references/plugin.yaml`:

```yaml
apiVersion: v1
name: references
version: 1.0.0
type: dependency/v1
runtime: wasi/preview1
```

The package contains one directory named after the plugin, with `plugin.yaml` and the fixed module path `plugin.wasm`. All five metadata fields are required; unknown or duplicate fields, aliases and extra YAML documents are rejected. `name` starts with an ASCII letter or digit and contains only ASCII letters, digits, `_` and `-`. `version` is strict SemVer, such as `1.0.0` or `1.0.0-rc.1`; the resulting `name-version.tgz` title must fit in 255 bytes. Add your `LICENSE` and applicable third-party license and notice files to that directory before packaging.

From a Bale checkout, use the pinned archive tools and publish the result:

```sh
nix develop --no-update-lock-file path:.#release --command \
  tar -czf references-1.0.0.tgz references
bale plugins push -f references-1.0.0.tgz registry.example.com/plugins/references:1
```

`bale plugins push` accepts the packaged `.tgz`, not a raw Wasm module. It publishes OCI artifact type `application/vnd.bale.plugin.v1+json`, with an empty JSON config and one `application/vnd.oci.image.layer.v1.tar+gzip` archive layer. The layer title is `name-version.tgz`. Publication preserves the package bytes, including license files. Bale validates the package and runs its module directly; Helm's installer does not support this runtime.

For existing registry artifacts, Bale also accepts an optional second provenance layer titled `name-version.tgz.prov`. It verifies the bytes without treating them as a trusted signature. An OCI index must select exactly one manifest with platform `wasm/wasip1`; the bundle retains the complete selected graph, including the index.

The archive is limited to 256 MiB in both compressed and expanded form, with at most 4,096 logical entries. `plugin.yaml`, OCI metadata and optional provenance are each limited to 4 MiB; the module remains limited to 128 MiB. Archive paths are limited to 4,096 bytes. Entries must be regular files or directories under the package root: links, special files, unsafe or duplicate paths, setuid/setgid permissions and trailing archive content are rejected. For the built-in Helm discovery package, `just helm-plugin-package` uses the reproducible Nix build.

## Runtime and cache boundaries

Bale uses Wago behind an internal adapter. The [Go API](../sdk.md#run-a-dependency-plugin) exposes no engine-specific types. Wago is a prerelease dependency; its native caches can be rebuilt from the module.

Create, authorized latest pull and manual generation accept these settings:

| Option | Default | Purpose |
| --- | --- | --- |
| `--plugin-cache DIR` | User cache directory under `bale/plugins` | Trusted compiled-code cache; `--plugin-cache ''` disables it. |
| `--plugin-memory BYTES` | `268435456` (256 MiB) | Guest linear-memory limit; multiple of 65,536 bytes, at most 4 GiB. |
| `--plugin-timeout DURATION` | `30s` | Guest execution timeout, excluding compilation. |

Helm's [`extism/v1` runtime](https://github.com/helm/helm/blob/v4.3.0/internal/plugin/runtime_extismv1.go) uses a different entry point and host APIs. Bale's discovery modules are not interchangeable with Helm Extism plugins. The native Bale Helm consumer uses Helm's `postrenderer/v1` subprocess contract instead.

Cached code is native executable content. Keep the cache local and trusted; do not distribute it as a plugin or include it in an OCI bundle. Distribute the portable plugin package instead. The cache directory must belong to the current user with mode `0700`; Bale creates it with that mode if absent. Cache entries must be regular files owned by the current user with mode `0600`; insecure or corrupt entries fail the run. The SDK disables persistent caching by default; callers opt in with `CacheDirectory`.

Disk caching requires supported Unix ownership and no-follow file operations. On other platforms, leave `CacheDirectory` empty or pass `--plugin-cache ''`.

Cache identity includes the host executable digest, module digest, platform and compiler settings. Rebuilding the host creates a separate cache automatically.

Runs serialize within one process. Waiting is cancellable, but compilation is synchronous: cancellation is checked before and after it. A cold compile can delay cancellation. The execution timeout applies only to the guest.

Guest linear-memory limits exclude the compiler, compiled code and host allocations. Measure large modules and apply host process limits when total memory must be bounded.
