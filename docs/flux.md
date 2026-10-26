# Deploy with Flux

Use `bale-flux` to deploy delivered charts through Flux Operator. The adapter reads destination records; a ResourceSet creates a digest-pinned OCIRepository and a HelmRelease with image mappings. Chart bytes stay unchanged.

Install Flux and [Flux Operator](https://fluxoperator.dev/) first. You also need Helm 4, kubectl and [delivered application charts](helm.md) with destination records.

## Build and deliver the adapter

Tagged releases publish the multiarchitecture `ghcr.io/strio-dev/bale-flux` image and `oci://ghcr.io/strio-dev/bale/charts/bale-flux` chart. For custom registries, [build and publish](development/releases.md#build-integrations) them with Nix; no container runtime is required.

Copy the [bootstrap recipe](examples/flux-bootstrap/bale.yaml) to `bale.yaml`. Replace `VERSION_OCI` with a release version, dropping the leading `v` and encoding `+` as `_`. It includes both the image and chart explicitly:

```sh
bale create -f bale.yaml -t bootstrap:v1 --output-delivery bale.delivery.yaml
```

Creation retains `bootstrap:v1` in the local store; push reads missing payloads from the pinned sources. Set their delivery paths to `images/bale-flux` and `charts/bale-flux`, then push:

```sh
bale push bootstrap:v1 mirror.example.com/release:bootstrap --delivery bale.delivery.yaml
```

## Configure and install

Save the application charts the adapter may resolve in `releases.yaml`:

```yaml
releases:
  - name: web
    chart: mirror.example.com/release/charts/web:1.4
    selection:
      context: production
```

Use a distinct release name of at most 63 lowercase letters, digits or hyphens, beginning and ending with a letter or digit. Each chart reference needs an explicit tag or digest and one matching local destination attachment and captured dependency resolution. Use the [capture selectors](reference/helm-discovery.md#select-a-capture) when several match. Requests cannot change this allowlist or combine captures.

The configuration accepts one YAML document and rejects unknown fields, duplicate keys, nulls, anchors and aliases. String values must be nonempty and have no surrounding whitespace.

Copy the [bootstrap values](examples/flux-bootstrap/values.yaml) to `values.yaml`. Set the service image's destination repository and tag. Create `flux-system` if needed and provision the [credentials and TLS Secrets](#private-registry-configuration), then install:

```sh
kubectl -n flux-system create configmap bale-flux-releases --from-file=releases.yaml
helm upgrade resolver oci://mirror.example.com/release/charts/bale-flux \
  --version VERSION --install --namespace flux-system --values values.yaml --wait
```

Use the release version without its leading `v` for `VERSION`, retaining any `+` build metadata. For a private CA, [log in with Helm](helm.md#private-registry-access) and add `--ca-file registry-ca.crt`. Plain Helm bootstraps the adapter using the image location in your values.

The example serves bearer-authenticated HTTPS at `https://resolver-bale-flux.flux-system.svc:8080`. Its certificate must cover that Service name. The chart installs the adapter Deployment and internal Service; install providers and ResourceSets separately.

Files load at startup. Chart-managed allowlist changes trigger a rollout; restart after changing an external ConfigMap or mounted Secret. `/healthz` checks the process without authentication or registry access. See the [chart settings](https://github.com/strio-dev/bale/tree/main/charts/bale-flux) for mounts and values.

## Connect Flux Operator

Copy the [provider and ResourceSet example](https://github.com/strio-dev/bale/blob/main/plugins/helm-relocate/examples/flux/resources.yaml). Set its provider URL to `https://resolver-bale-flux.flux-system.svc:8080/inputs/web`, then adjust namespaces, values and Secret names:

```sh
kubectl apply -f resources.yaml
```

The ResourceSet waits for the pinned OCIRepository, then updates the HelmRelease's `chartRef` and image post-renderers together. Keep normal HelmRelease settings in its template. Include custom deployment images explicitly in the bundle and set their destination references in `HelmRelease.spec.values`.

Give each release one owning ResourceSet and a distinct name in its namespace. Git manages the provider and ResourceSet; Flux Operator owns their generated resources. Old sources are pruned after the final step is applied, before the new HelmRelease is ready.

`GET /inputs/web` resolves one release; `GET /inputs` resolves all atomically. For the aggregate endpoint, set provider `spec.filter.limit` to at least your release count; Flux Operator defaults to 100, while the adapter allows 256.

Failed or ambiguous lookups return errors rather than empty or partial results. Flux Operator retains its last successful exported inputs on fetch failure.

## Private registry configuration

Configure each connection separately:

| Client | Credentials | CA trust |
| --- | --- | --- |
| Adapter → registry | `registry.existingSecret` and `configKey` | `registry.ca.existingSecret` |
| Flux source-controller → registry | OCIRepository `secretRef` | OCIRepository `certSecretRef` |
| Kubelet → registry | Pod `imagePullSecrets` | Node container-runtime trust |
| Provider → adapter | Provider `secretRef` with `token` | Provider `certSecretRef` |

Prepare `registry-config.json` with Docker-compatible `auths` for the exact destination host, including its port, and `registry-ca.crt`. The adapter image cannot run credential helpers; supply actual credentials scoped to the required repositories. Keep passwords, tokens and private keys outside bundles and Git.

For the example namespaces, create either namespace only if it does not exist:

```sh
kubectl create namespace flux-system
kubectl create namespace apps
for namespace in flux-system apps; do
  kubectl -n "$namespace" create secret generic destination-registry-auth \
    --type=kubernetes.io/dockerconfigjson \
    --from-file=.dockerconfigjson=registry-config.json
  kubectl -n "$namespace" create secret generic destination-registry-ca \
    --from-file=ca.crt=registry-ca.crt
done
```

Bootstrap values mount these files into the adapter and set its Pod's `imagePullSecrets`. The ResourceSet example configures the generated OCIRepository. Flux accepts a CA-only `certSecretRef`; add client certificates when your registry requires mutual TLS. See [Flux authentication](https://fluxcd.io/flux/components/source/ocirepositories/#secret-reference) and [TLS configuration](https://fluxcd.io/flux/components/source/ocirepositories/#mutual-tls-authentication).

Set workload pull Secrets in each Pod's namespace through chart values, including hooks and subcharts. Install the registry CA in every relevant node's [runtime trust configuration](https://github.com/containerd/containerd/blob/f2551031d7276a770f65f98c9b52e57e7dad07e8/docs/hosts.md#cri); Pod Secrets do not configure node trust.

For provider-to-adapter HTTPS, prepare `resolver.crt`, `resolver.key`, `resolver-ca.crt` and `resolver-token`. The certificate must cover `resolver-bale-flux.flux-system.svc`:

```sh
kubectl -n flux-system create secret tls bale-resolver-tls \
  --cert=resolver.crt --key=resolver.key
for namespace in flux-system apps; do
  kubectl -n "$namespace" create secret generic bale-resolver-auth \
    --from-file=token=resolver-token
done
kubectl -n apps create secret generic bale-resolver-ca \
  --from-file=ca.crt=resolver-ca.crt
```

The provider uses the token and CA in `apps`; the adapter uses the matching token and server certificate in `flux-system`. Provider `insecure: true` permits HTTP for local tests; it does not disable TLS verification.

## Run the binary directly

For a host deployment, [build the service](development/releases.md#build-integrations) and use the same allowlist:

```sh
result-flux-service/bin/bale-flux --config releases.yaml \
  --listen 127.0.0.1:8080 --registry-config /run/secrets/registry/config.json
```

For network access, configure `--tls-cert-file`, `--tls-key-file` and `--bearer-token-file`, or terminate TLS at an ingress. Registry TLS uses separate `--ca-file`, `--cert-file` and `--key-file` options. Repeated `--plain-http HOST` permits HTTP for exact registry hosts. Restart after config changes; diagnostics go to stderr.

## Coverage and limits

The adapter emits exact Kustomize image rules for captured references, distinct tags, digests and Docker Hub shorthand. Flux handles supported workload fields and hooks; valid unmapped images stay unchanged. Custom resources need schema-aware handling. Include custom deployment images explicitly; Bale does not check deployment values during capture.

The integration targets Flux Operator **0.61.0**, helm-controller **1.6.5** and Kustomize API **0.21.1**. Qualify controller upgrades against your chart profiles.

| Limit | Value |
| --- | --- |
| Configuration | 1 MiB; 256 releases |
| Chart metadata inspection | 256 advertised referrers; 4 MiB inventory and attachment reads |
| Serialized destination records | 4 MiB |
| Resolution | 30 seconds per request; 16 concurrent requests |
| Response | 900 KiB; no partial success |

Excess concurrent requests receive HTTP 503. These are request budgets, not process memory limits. The service reads metadata without a chart renderer, container/Wasm runtime or cluster API client.
