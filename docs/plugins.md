# Dependency plugins

Run a registry-hosted plugin during bundle creation to discover an artifact's dependencies. Bale captures the plugin package, its generated [declaration](dependencies.md) and the resolved artifacts together.

For charts, follow the [Helm guide](helm.md). To write a plugin, see the [WASI protocol and Go example](reference/plugin-runtime.md).

## Configure discovery in a recipe

Select a published plugin on the artifact it understands:

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

Replace the chart and plugin references. Each needs an explicit registry and tag or digest; pin the plugin digest to keep its code fixed across new captures.

`input` supplies the plugin's JSON object, authored in YAML, and defaults to `{}`. `blobs` supplies verified artifact-layer bytes to fields such as `chart`, matched by media type and encoded as base64. Each binding must match one layer and cannot replace a field already in `input`.

For Helm, this discovers chart-default images. Add deployment-specific images as ordinary recipe entries and use their destination references in deployment values. See the [offline Helm example](examples/helm-offline/README.md).

An input with a plugin cannot also set `dependencies.follow: none` or `dependencies.select`. Use separate inputs for different discovery profiles.

## Capture and refresh

Creation runs discovery and resolves the returned references. It saves the declaration in the bundle without writing to the source registry. Dry-run creation also runs discovery, but retains no content.

Locked pull, inspection and push use the capture. To rerun discovery while refreshing tags, authorize plugins explicitly:

```sh
bale pull registry.example.com/bundles/release:v1 ./refreshed --mode latest --allow-plugins
```

## Publish a plugin

[Package a WASI module](reference/plugin-runtime.md#package-for-publication) with its metadata and license files, then publish the archive:

```sh
bale plugins push -f references-1.0.0.tgz registry.example.com/plugins/references:1
```

The package follows Helm's archive layout with Bale-specific execution metadata. It runs in Bale, not Helm. Registry credentials follow [registry access](registry.md).

## Reuse a delivered plugin

In the next recipe, select the plugin's delivered repository and tag or unchanged digest. Use destination references for the artifacts and any references in plugin input too. The package runs without its original registry or a preexisting compilation cache.

The new capture generates a fresh declaration. Earlier recipes and declarations remain in their original bundles. See [Helm reuse](helm.md#reuse-after-transfer).

## Generate or attach a declaration manually

Prepare a JSON request following the plugin's schema, including any required archive bytes. Preview its declaration, then attach it:

```sh
bale dependencies generate --plugin registry.example.com/plugins/references:1 --input request.json > dependencies.yaml
bale dependencies attach registry.example.com/charts/web:1.4 -f dependencies.yaml
```

Or generate and attach in one command:

```sh
bale dependencies attach registry.example.com/charts/web:1.4 --plugin registry.example.com/plugins/references:1 --input request.json
```

References resolve during bundle creation. For local development, use `--plugin-file references.wasm` instead of `--plugin`; both require `--input`.

## Runtime and cache boundaries

Plugins exchange JSON over WASI streams without host filesystem, environment or network access. Bale uses Wago; Helm Extism modules use a different execution contract. See [runtime settings and limits](reference/plugin-runtime.md#runtime-and-cache-boundaries) for cache, memory and timeout options.
