# Bundle artifacts. Move them together.

<div class="strio-intro" markdown>
Bale captures OCI images and artifacts in a portable bundle.
Carry an offline archive or store it in a registry, then push where you choose.
</div>

[Get started](getting-started.md){ .md-button .md-button--primary }
[Inspect a bundle](inspection.md){ .md-button }
{ .strio-actions }

<div class="strio-paths" markdown>
<div class="strio-path" markdown>
<p class="strio-path__title">Move a bundle</p>

Capture related images and artifacts, carry them as an OCI layout, and choose the destination registry when you push.

[Bundle walkthrough](getting-started.md)
</div>
<div class="strio-path" markdown>
<p class="strio-path__title">Use the Go SDK</p>

Build integrations with caller-owned stores, dependency declarations, and confirmed artifact relocation results.

[SDK guide](sdk.md)
</div>
</div>

## Capture once, move together

List the images and artifacts you need in `bale.yaml`, using references you can read:

```yaml
include:
  - registry.example.com/apps/api:stable
  - registry.example.com/apps/worker:1.4
```

Create a local bundle, save it for transfer, then restore and push it to the receiving registry:

```sh
bale create --layout -f bale.yaml ./release
bale save ./release -o release.tar
# On the receiving side:
bale load -i release.tar ./restored
bale push ./restored mirror.example.com/releases/v1:bundle
```

Carry `release.tar` across the offline boundary. [Follow the offline walkthrough](offline.md), or read about [destination mappings](transfers.md#push-to-a-destination).

## Keep related content together

- **Preserve original digests.** Capture complete multiarchitecture images, OCI artifacts, nested bundles and selected referrers. [Write a recipe](recipes.md).
- **Carry declared dependencies.** Include the references an artifact needs and keep their captured resolutions for locked transfers. [Declare dependencies](dependencies.md) or [generate them with a plugin](plugins.md).
- **Discover Helm images.** Select a registry plugin in the recipe to render a chart profile offline and capture images from workloads, hooks, and subcharts. [Prepare a chart](helm.md).
- **Deploy delivered charts.** Read destination attachments through the [Helm integration](helm.md#use-delivered-images-in-helm) or [Flux Operator adapter](flux.md).
- **Inspect before delivery.** Read verified inventories, selected declarations and resolved digests from a registry or local layout. [Inspect a bundle](inspection.md).

See [registry behavior](registry.md) for authentication and retention settings, and [contributing](contributing.md) for source builds and documentation checks.
