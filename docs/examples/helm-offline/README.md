# Carry a chart and deploy its delivered images

This example captures a packaged OCI chart, discovers its default images through the OCI-hosted Helm Wasm plugin, and carries an explicitly included custom image and the native Helm integration in the same bundle. The receiving side needs Bale, Helm 4 and access to the destination registry and cluster. Capture, save and load need no container runtime or cluster.

Copy [bale.yaml](bale.yaml) to a new working directory. The registry names are placeholders: supply your own published chart, custom image and plugins before running these commands. Publish the discovery module and matching native package as described in the [Helm guide](../../helm.md#publish-and-configure-discovery) and [package instructions](../../helm.md#package-and-transfer). The native tag here selects Linux AMD64; use the package for your deployment machine.

Discovery uses the chart's default values, release `web`, namespace `apps` and context `production`. The recipe separately includes `registry.example.com/images/custom-web:2` for a later deployment override. Bale does not read deployment values during capture. Discovery includes supported rendered workloads, init containers and lifecycle hooks from bundled subcharts. All declared subchart dependencies must already be inside the chart archive; discovery never downloads missing charts or executes hooks.

The deployment example assumes a chart that accepts a complete image reference at `image`. Adapt [values.yaml](values.yaml) to your chart's schema; charts using `image.repository` and `image.tag` need those separate fields instead.

## Capture and choose paths

On the source-connected machine, run:

```sh
bale create --layout -f bale.yaml ./release \
  --output-delivery bale.delivery.yaml
bale inspect --layout ./release
```

Chart defaults need no separate image inventory or source attachment. Images introduced by custom deployment values need ordinary `include` entries, as shown in the recipe. Review the captured artifacts and edit only `path` values in the generated delivery file:

| Artifact | Delivery path |
| --- | --- |
| Chart `registry.example.com/charts/web` | `charts/web` |
| Native Helm plugin `registry.example.com/plugins/bale` | `plugins/bale` |
| Discovery plugin `registry.example.com/plugins/helm` | `plugins/helm` |
| Custom image `registry.example.com/images/custom-web` | `images/custom-web` |
| Discovered images | Optional paths such as `images/web`; defaults also work |

Keep the generated `bundle` digest and artifact `ref` values intact. Preserve the native plugin repository's final name `bale`, which Helm uses to locate its archive. A chart name other than `web` needs a matching chart path and deployment reference. Images and plugins keep their captured tags; no destination registry belongs in the recipe.

Preview placement and save the bundle:

```sh
bale push ./release mirror.example.com/release:bundle \
  --delivery bale.delivery.yaml --dry-run
bale save ./release -o release.tar
```

Carry `release.tar` and the edited `bale.delivery.yaml` to the receiving side. Layout and archive output paths must be new. The delivery file remains separate so placement can change without changing captured content.

## Restore, deliver and deploy

The source registries can now be offline:

```sh
bale load -i release.tar ./restored
bale push ./restored mirror.example.com/release:bundle \
  --delivery bale.delivery.yaml --dry-run
bale push ./restored mirror.example.com/release:bundle \
  --delivery bale.delivery.yaml
```

After delivery, write `values.yaml` with the custom image's transferred location:

```yaml
image: mirror.example.com/release/images/custom-web:2
```

This is a deployment file you author, not Bale output. Bale does not verify that every image override was captured. Configure any chart-specific image-pull credentials separately.

Install the transferred plugin and deploy:

```sh
helm plugin install oci://mirror.example.com/release/plugins/bale:0.0.0-linux-amd64 \
  --verify=false
helm bale --context production upgrade web \
  oci://mirror.example.com/release/charts/web:1.4 \
  --install --namespace apps --create-namespace --values values.yaml --wait
```

The development native package is unsigned, hence `--verify=false`; apply your plugin verification policy for signed packages. Install into a Helm environment without another `bale` plugin already installed. `helm bale` uses the delivered chart's destination attachment to substitute mapped source references, including supported init containers and hooks. It leaves valid unmapped references unchanged, so the custom destination image passes through without an extra attachment. The chart archive stays unchanged. Use ordinary `helm rollback`, `helm history` and `helm uninstall` afterward.

The destination values file also works with plain Helm; plain Helm will not relocate any remaining source references from the chart defaults. Use `helm bale` or the Flux integration when those defaults still need mapping.

For private registries, Bale uses Docker credentials, including `DOCKER_CONFIG`; trust a private CA through the system trust store or `SSL_CERT_FILE`. Bale has no `--ca-file` flag. Helm uses its registry configuration and `--ca-file`; use the [aligned Helm credential location](../../helm.md#private-registry-access) for authenticated OCI plugin installation. Configure Pod pull credentials and node trust separately, including hooks and subcharts; follow the [private registry guide](../../flux.md#private-registry-configuration).

The same delivered chart and attachment work with the [Flux integration](../../flux.md). Set the custom destination image in its HelmRelease values. The [optional Kubernetes acceptance test](../../contributing.md#kubernetes-acceptance) exercises the CLI-to-cluster workflow with pinned local fixtures; the placeholder chart, custom image and registries in this example must be supplied by you.
