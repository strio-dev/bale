# Dependency plugins

Run a registry-hosted discovery plugin during bundle creation to find an artifact's dependencies. Bale captures the portable Wasm module, resolved digest, generated [declaration](dependencies.md) and dependency graphs together.

For chart discovery and deployment, follow the [Helm guide](helm.md). To write a plugin, use the [WASI protocol and Go example](reference/plugin-runtime.md).

## Configure discovery in a recipe

Publish your plugin first, then select it on the artifact that it understands:

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

```sh
bale create --layout -f bale.yaml ./release
```

Replace the registry references with your published chart and plugin. `plugin.ref` requires an explicit registry and tag or digest. Pin its digest when fresh captures must use fixed plugin content.

`input` is the plugin's JSON object, authored in YAML; it defaults to `{}`. Supply values directly. Nested nulls, booleans and numbers retain their JSON meanings. `blobs` binds top-level input fields to artifact layers by media type. Each binding must match exactly one layer; Bale verifies and base64-encodes its bytes. A bound field cannot also appear in literal `input`.

For Helm, this example discovers the chart defaults. Add custom deployment images as ordinary recipe `include` entries, then use their destination references in deployment values. See the [Helm offline example](examples/helm-offline/README.md).

Each input selects one plugin. It cannot also set `dependencies.follow: none` or `dependencies.select`. Separate inputs can use different profiles of one artifact.

## Capture and refresh

Creation runs discovery and resolves the returned dependencies. It stores the declaration in the bundle without publishing an attachment to the source registry. The captured recipe keeps literal inputs and blob bindings, rather than injected archive bytes. A dry run executes discovery but publishes nothing.

Locked pull, inspection and push use captured content. Latest pull reruns discovery only with explicit authorization:

```sh
bale pull registry.example.com/bundles/release:v1 ./refreshed --mode latest --allow-plugins
```

Progress and errors identify the source input and selected plugin. Use `--quiet` to suppress progress; see [CLI output](reference/cli.md).

## Publish a plugin

Build a WASI Preview 1 command module, then publish it:

```sh
bale plugins push -f references.wasm registry.example.com/plugins/references:1
```

The OCI image has an `application/vnd.wasm.config.v0+json` config with `architecture: wasm`, `os: wasip1` and its ordered layer digests, plus one `application/wasm` layer. An index must select exactly one `wasm/wasip1` manifest. Bundles retain the selected root graph, including an index when present. Registry credentials and HTTP policy follow [registry access](registry.md).

## Reuse a delivered plugin

In a new recipe, select the plugin's destination repository and restored tag or unchanged digest. Use destination references for the artifacts and any references in plugin input too. The portable module runs with an empty local compilation cache; it does not need the original registry.

Delivery preserves captured recipes and declarations. Configured discovery generates a fresh declaration for the new capture, selected instead of historical source declarations. See [Helm reuse](helm.md#reuse-after-transfer).

## Generate or attach a declaration manually

For a preview, pass a plugin reference and its JSON request:

```sh
bale dependencies generate --plugin registry.example.com/plugins/references:1 --input request.json > dependencies.yaml
bale dependencies attach registry.example.com/charts/web:1.4 -f dependencies.yaml
```

The request must follow the selected plugin's schema and already contain required archive bytes. Generate validates the response and writes authoring YAML; dependency references resolve during bundle creation. Attach can generate and publish in one command:

```sh
bale dependencies attach registry.example.com/charts/web:1.4 --plugin registry.example.com/plugins/references:1 --input request.json
```

During development, replace `--plugin OCIREF` with `--plugin-file references.wasm`. Both require `--input`.

## Runtime and cache boundaries

Plugins receive JSON over WASI streams with no host filesystem, environment or network access. They need no container runtime. Execution uses Wago through an internal adapter; Helm's Extism modules use a different ABI and are not interchangeable.

Create, authorized latest pull and manual generation accept these host settings:

| Option | Default | Purpose |
| --- | --- | --- |
| `--plugin-cache DIR` | User cache directory under `bale/plugins` | Trusted local compiled-code cache; `--plugin-cache ''` disables it. |
| `--plugin-memory BYTES` | `268435456` (256 MiB) | Guest linear-memory limit; multiple of 65,536 bytes, at most 4 GiB. |
| `--plugin-timeout DURATION` | `30s` | Guest execution timeout, excluding compilation. |

Input is limited to 8 MiB, stdout to 4 MiB, stderr to 64 KiB and the module to 128 MiB. The guest limit does not cap compiler or total process memory. Cold compilation can delay cancellation. Runs serialize within a process.

Cached code is executable and must stay local and trusted; transfer the original Wasm instead. The [runtime reference](reference/plugin-runtime.md#runtime-and-cache-boundaries) covers cache permissions, portability, cancellation and the SDK's opt-in cache policy.
