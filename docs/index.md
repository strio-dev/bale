# Bundle artifacts. Move them together.

<div class="strio-intro" markdown>
Bale captures OCI images and artifacts in a portable bundle.
Carry an offline archive or store it in a registry, then push where you choose.
</div>

[Get started](getting-started.md){ .md-button .md-button--primary }
[Create your first bundle](tutorials/first-bundle.md){ .md-button }
{ .strio-actions }

## Install Bale

```sh
curl -fsSL https://bale.strio.dev/install.sh | sh
bale --version
```

See [installation](getting-started.md) for requirements, exact versions and alternative installs.

<div class="strio-paths" markdown>
<div class="strio-path" markdown>
<p class="strio-path__title">Carry an offline bundle</p>

Capture an image, inspect its contents, and restore it from an archive. Choose a destination registry when you deliver.

[First-bundle tutorial](tutorials/first-bundle.md)
</div>
<div class="strio-path" markdown>
<p class="strio-path__title">Move a Helm application</p>

Discover a chart's images, transfer them together, and deploy using their destination locations.

[Helm guide](helm.md)
</div>
</div>

## Capture once, move together

List the artifacts you need in `bale.yaml`:

```yaml
include:
  - docker.io/library/busybox:1.37
```

Capture their current digests and save a portable archive:

```sh
bale create --layout -f bale.yaml ./release
bale save ./release -o release.tar
```

On the receiving side, restore the archive and push to your registry:

```sh
bale load -i release.tar ./restored
bale push ./restored mirror.example.com/releases/v1:bundle
```

Replace `mirror.example.com` with a registry you can write to. [Follow the tutorial](tutorials/first-bundle.md) for inspection and a delivery preview before publication.

## Keep related content together

- **Preserve original content.** Capture complete multiarchitecture images, OCI artifacts, nested bundles and selected referrers. [How bundles work](concepts/bundles.md).
- **Include dependencies.** Follow attached declarations or run an OCI discovery plugin selected in your recipe. [Recipes](recipes.md) · [Dependency declarations](dependencies.md).
- **Deploy delivered charts.** Read destination records through the [Helm integration](helm.md#use-delivered-images-in-helm) or [Flux Operator adapter](flux.md).
- **Build your own integration.** Use caller-owned stores, dependency declarations and transfer plans through the [Go SDK](sdk.md).
