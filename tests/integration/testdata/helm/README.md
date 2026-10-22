# Helm integration fixture

`podinfo-6.9.4.tgz` is the unchanged upstream Podinfo Helm chart from
[stefanprodan/podinfo](https://github.com/stefanprodan/podinfo), published at
`oci://ghcr.io/stefanprodan/charts/podinfo:6.9.4`.
Its SHA-256 is `d2caf247bdb0b3c7813e1b26d235245aca90794f94a133c0101cec4857d2722d`.
The archive includes its upstream Apache-2.0 `podinfo/LICENSE`.

The optional `just kubernetes-acceptance` workflow also uses this unchanged
archive with real upstream Linux AMD64 images. Nix fetches Podinfo 6.9.4 and
6.9.3 plus the three default test-hook images (`curlimages/curl`,
`giantswarm/tiny-tools` and `stefanprodan/grpc_health_probe`) by fixed digest and
archive hash. Inside the guest, an isolated native registry mirror serves those
images under their original `ghcr.io` and `docker.io` names. It does not modify
public registry tags.

Capture uses chart defaults without deployment values; Podinfo 6.9.3 is an
explicit ordinary recipe image. The mirror and source registry stop before
archive load and delivery. Native Helm and Flux then deploy 6.9.4, select the
delivered 6.9.3 image through deployment-only values, and roll back or restore
6.9.4. Pod image IDs and `/version` responses verify the running version. This
live case complements the synthetic main, init-container, subchart-hook and
credential-failure coverage; it does not qualify other image architectures.

`TestHelmPluginPodinfo` checks default and digest-pinned image discovery,
including Podinfo's rendered test hooks, against independently verified expected
references. It does not invoke native Helm or contact the registry.

`TestHelmPluginPodinfoOfflineDelivery` transfers this real chart through the
complete attachment, bundle, locked pull and delivery cycle. Its main image is
set through Helm values; original public hook selectors retain their provenance
while a test-only HTTP transport serves isolated fixture image graphs. Other
remote hosts are denied. Source and holding registries are shut down before the
next stage, and final chart bytes and dependency mappings are checked.

`TestHelmPluginOfflineBundleDelivery` constructs a parent chart with a bundled
child and main, init, ephemeral and hook containers. It runs the production Wasm
producer, publishes its generic declaration, captures a chart-only recipe,
shuts down the source, pulls locked from holding, shuts down holding, and delivers
to an isolated destination. It checks deduplication, both image platforms,
unchanged chart bytes and destination dependency mappings. Deployment consumer
rewriting is outside this test.

These tests run when `BALE_HELM_PLUGIN` identifies the built production Wasm
plugin. Only an unset variable skips them; an invalid supplied path fails.
The Nix Helm integration check supplies the artifact automatically.
