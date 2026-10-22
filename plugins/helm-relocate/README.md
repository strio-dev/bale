# Helm image relocation

A native Helm 4 CLI plugin and Flux Operator service that apply Bale destination
attachments after delivery. Discovery captures dependencies before transfer.

Build and install from the Bale repository root:

```sh
nix build --no-update-lock-file path:.#helm-consumer --out-link result-helm-consumer
helm plugin install result-helm-consumer/share/helm/plugins/bale
```

Run `helm bale upgrade web oci://mirror.example.com/charts/web:1.4 --install`.
The adapter selects an attachment, pins the chart, and invokes Helm with its
private native renderer. See [the Helm guide](../../docs/helm.md) for selection
and limits, or [the Flux guide](../../docs/flux.md) for the HTTP service.

Both integrations rewrite mapped references and leave valid unmapped images
unchanged. Include custom deployment images explicitly in the bundle, then set
their destination references in ordinary Helm values or Flux HelmRelease values.
