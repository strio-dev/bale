# Bundle artifacts. Move them together.

<div class="strio-intro" markdown>
Bale captures OCI images and artifacts in a portable bundle.
Capture related content once, then carry it offline or deliver it through a registry.
</div>

[Get started](getting-started.md){ .md-button .md-button--primary }
[Create your first bundle](tutorials/first-bundle.md){ .md-button }
{ .strio-actions }

## Install Bale

```sh
curl -fsSL https://bale.strio.dev/install.sh | sh
bale --version
```

See [getting started](getting-started.md) for requirements, the Bale container and other installation options.

<div class="strio-paths" markdown>
<div class="strio-path" markdown>
<p class="strio-path__title">Carry an offline bundle</p>

Capture an image, inspect the bundle and carry it to another machine.

[First-bundle tutorial](tutorials/first-bundle.md)
</div>
<div class="strio-path" markdown>
<p class="strio-path__title">Move a Helm application</p>

Carry a chart and its images together, then deploy from the receiving registry.

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

The archive contains everything captured by the bundle. [Follow the tutorial](tutorials/first-bundle.md) to restore it, choose delivery paths and push to a receiving registry.

## Keep related content together

- **Carry complete artifacts.** Keep image platforms and related OCI content together. [How bundles work](concepts/bundles.md).
- **Include dependencies.** Capture the images and artifacts an application needs. [Recipes](recipes.md).
- **Deploy from your registry.** Use delivered image locations with [Helm](helm.md) or [Flux](flux.md).
- **Integrate with other tools.** Use Bale as a library through the [Go SDK](sdk.md).
