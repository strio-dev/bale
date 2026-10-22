# Transfer a chart and deploy offline

Carry a chart, its default images, a custom image and both Bale Helm plugins to another registry. The capture machine needs Bale and access to the source registry; the deployment machine needs Bale, Helm 4, destination registry access and a cluster.

## Prepare the recipe

Build and publish your [discovery plugin](../../helm.md#publish-and-configure-discovery) and [native Helm plugin](../../helm.md#package-and-transfer). Copy [bale.yaml](bale.yaml) into a working directory and replace its placeholder references with your published chart, plugins and custom image. The native tag selects Linux AMD64; use a package matching your deployment machine.

Discovery renders chart defaults with release `web`, namespace `apps` and context `production`. All declared subcharts must already be packaged in the chart. Hooks are inspected, never executed during capture. The custom image is an ordinary explicit include for a later deployment override.

## Capture and choose paths

```sh
bale create --layout -f bale.yaml ./release --output-delivery bale.delivery.yaml
bale inspect --layout ./release
```

Review the inventory and edit `path` values in `bale.delivery.yaml`:

| Artifact | Delivery path |
| --- | --- |
| Chart `registry.example.com/charts/web` | `charts/web` |
| Native plugin `registry.example.com/plugins/bale` | `plugins/bale` |
| Discovery plugin `registry.example.com/plugins/helm` | `plugins/helm` |
| Custom image `registry.example.com/images/custom-web` | `images/custom-web` |
| Discovered images | Keep generated paths, or choose your own |

Keep the bundle digest and artifact references intact. Preserve the native plugin's final repository name `bale`, which Helm uses to find the archive. If your chart has another name, adapt its path and deployment reference too.

```sh
bale push ./release mirror.example.com/release:bundle --delivery bale.delivery.yaml --dry-run
bale save ./release -o release.tar
```

Carry `release.tar` and the edited delivery file to the receiving machine. Archive and layout output paths must be new. Placement stays separate from captured content.

## Restore and deliver

The source registries can now be offline:

```sh
bale load -i release.tar ./restored
bale push ./restored mirror.example.com/release:bundle --delivery bale.delivery.yaml --dry-run
bale push ./restored mirror.example.com/release:bundle --delivery bale.delivery.yaml
```

## Write values and deploy

Write destination deployment values after transfer. The included [values.yaml](values.yaml) assumes your chart accepts a complete image reference at `image`:

```yaml
image: mirror.example.com/release/images/custom-web:2
```

Adapt this to your chart's schema, such as separate `image.repository` and `image.tag`. Bale does not generate these values or check every deployment override; include each required custom image explicitly before capture.

Install into a Helm environment without an existing `bale` plugin:

```sh
helm plugin install oci://mirror.example.com/release/plugins/bale:0.0.0-linux-amd64 --verify=false
helm bale --context production upgrade web \
  oci://mirror.example.com/release/charts/web:1.4 \
  --install --namespace apps --create-namespace --values values.yaml --wait
```

The development package is unsigned, hence `--verify=false`. The adapter maps captured chart defaults to destination image digests, including supported hooks and init containers. Your explicit destination override passes through unchanged. Chart bytes stay intact; use ordinary Helm commands for rollback, history and uninstall.

Plain Helm accepts the same values but will not relocate remaining source references in chart defaults. The [Flux adapter](../../flux.md) applies the same mappings through a ResourceSet; put the custom destination image in HelmRelease values.

For private registries, configure [Bale credentials and trust](../../registry.md), [Helm's installer credential location](../../helm.md#private-registry-access), and [workload pull credentials and node trust](../../flux.md#private-registry-configuration) separately.
