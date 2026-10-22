# Flux Operator adapter

`bale-flux` resolves configured chart attachments and supplies inputs to Flux
Operator's `ExternalService` provider. It neither renders charts nor changes
Bale push behavior. This example targets Flux Operator 0.61.0 and Flux
helm-controller 1.6.5 (Kustomize 0.21.1).

Start the resolver with the example allowlist adapted to your registry:

```sh
bale-flux --config releases.yaml --listen 127.0.0.1:8080 \
  --registry-config /run/secrets/registry/config.json
```

`GET /inputs/web` resolves only `web`; `GET /inputs` resolves every configured
release atomically. Requests cannot supply arbitrary registry references or
selection overrides. Missing, ambiguous, failed, and oversized results return a
non-success status, never successful empty inputs. Flux Operator retains its
previous successful inputs on fetch failure. A tag is resolved to one chart
digest and one physical attachment/capture per request; no latest-record policy
or capture union is used.

For a network-facing service use `--tls-cert-file`/`--tls-key-file` and
`--bearer-token-file`, or terminate TLS at your ingress. The provider Secret in
`resources.yaml` needs a `token` entry matching the token file. Provider
`.spec.insecure: true` permits HTTP for a local test; it is not a TLS verification
bypass. Registry options `--ca-file`, `--cert-file`, `--key-file`, and repeated
`--plain-http HOST` are independent of server TLS.

Git manages the provider and ResourceSet; Flux Operator owns generated sources
and HelmReleases. Step one prepares a repository-and-digest-addressed chart
source scoped to the release and waits for readiness. Step two updates the stable release's chart
reference and post-renderers together. Previous sources are pruned after the
final step is applied, before final HelmRelease readiness. Give each release one owning ResourceSet and a distinct name
in its namespace. Keep values and other normal HelmRelease settings in the
ResourceSet template. Adapt the example's three separate credentials: resolver
registry access, provider-to-resolver token, and Flux source registry Secret.
Workload image-pull credentials are also a cluster concern.

For a private CA, create the same-namespace Secrets referenced by
`resources.yaml`: `destination-registry-auth` is a
`kubernetes.io/dockerconfigjson` Secret, and `destination-registry-ca` contains
`ca.crt`. Provider Secrets `bale-resolver-auth` (`token`) and
`bale-resolver-ca` (`ca.crt`) authenticate the separate resolver HTTPS connection.
The [private registry configuration](../../../../docs/flux.md#private-registry-configuration)
shows creation commands, resolver mounts, Helm login and node/containerd trust.
Adapt the example workload `imagePullSecrets` value to the chart, including
hooks and subcharts; the resolver does not add pull credentials.

The image mappings use exact reference regular expressions to distinguish two
tags in the same repository, preserve pinned digests, and support Docker Hub
shorthand aliases. The service has no rendered YAML: **unmapped images are left
unchanged**, including references introduced by different values or operators.
The native Helm renderer also leaves valid unmapped images unchanged. Explicitly
include images introduced by custom deployment values in the bundle, then set
their destination references in the ResourceSet's HelmRelease values. This needs
no extra chart attachment or deployment values during capture. Exercise chosen
chart values and hooks before deployment. The regexp integration relies
on the specified Kustomize image-matching behavior and needs qualification when
upgrading that implementation.
