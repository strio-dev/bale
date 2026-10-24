# Helm image relocation

A native Helm 4 CLI plugin and Flux Operator service that apply Bale destination
attachments after delivery. Discovery captures dependencies before transfer.

Use Helm 4 on Linux AMD64 or ARM64. From the Bale repository root, build a
package for the current machine with Nix, then install it into a Helm environment
without an existing `bale` plugin:

```sh
nix build --no-update-lock-file path:.#helm-consumer-archive --out-link result-helm-consumer-archive
helm plugin install result-helm-consumer-archive/bale-0.0.0.tgz --verify=false
```

The development archive is unsigned. For versioned builds, use the generated
archive's version in its filename. See [integration packaging and publication](../../docs/development/releases.md)
to transfer the package to another machine.

After [delivering a chart and its images](../../docs/helm.md), preview it with:

```sh
helm bale template web oci://mirror.example.com/release/charts/web:1.4
```

The adapter selects an attachment, pins the chart, and invokes Helm with its
private native renderer. See [the Helm guide](../../docs/helm.md) for selection
and limits, or [the Flux guide](../../docs/flux.md) for the HTTP service.

Both integrations rewrite mapped references and leave valid unmapped images
unchanged. Include custom deployment images explicitly in the bundle, then set
their destination references in ordinary Helm values or Flux HelmRelease values.
