# Deploy with Flux

Use `bale-flux` to turn a delivered chart's destination attachments into Flux inputs. A Flux Operator ResourceSet creates a digest-pinned OCIRepository and a HelmRelease with image mappings; chart bytes stay unchanged.

This integration requires [Flux Operator](https://fluxoperator.dev/) in addition to Flux. Install those controllers first. The commands below also require Helm 4, Skopeo and kubectl with access to your cluster. The adapter reads registry metadata; Flux renders and deploys the chart.

## Build and deliver the adapter

From a Bale checkout with [Nix set up](contributing.md), build the service image and bootstrap chart:

```sh
just flux-image-build
just flux-chart-build
```

These builds need no container runtime. The image targets the current Linux platform; the flake supports AMD64 and ARM64. Publish your development artifacts with the pinned release tools:

```sh
nix develop --no-update-lock-file path:.#release --command \
  skopeo copy oci:result-flux-service-image:dev \
  docker://registry.example.com/images/bale-flux:dev
nix develop --no-update-lock-file path:.#release --command \
  helm push result-flux-service-chart/bale-flux-0.0.0-dev.tgz \
  oci://registry.example.com/charts
```

Copy the [bootstrap recipe](examples/flux-bootstrap/bale.yaml) to `bale.yaml` and replace its registry names with your published references. The recipe explicitly includes both artifacts; bootstrap needs no discovery plugin.

```sh
bale create --layout -f bale.yaml bootstrap --output-delivery bale.delivery.yaml
```

Set `charts/bale-flux` and `images/bale-flux` as their paths in the generated delivery file, then push:

```sh
bale push bootstrap mirror.example.com/release:bootstrap --delivery bale.delivery.yaml
```

## Configure and install

Write `releases.yaml` with the delivered application charts the adapter may resolve:

```yaml
releases:
  - name: web
    chart: mirror.example.com/release/charts/web:1.4
    selection:
      context: production
```

Each chart must already have a delivered destination attachment. Selectors use the [Helm selection rules](helm.md#selection-and-validation): exactly one local attachment and one captured dependency resolution. Add `bundle`, `source`, `capture` or `declaration` when needed. Selection never chooses the newest record or unions captures. Requests cannot override this allowlist.

Copy the [bootstrap values](examples/flux-bootstrap/values.yaml) to `values.yaml`. Set the service image to its destination repository and tag. Create the `flux-system` namespace and the [referenced credentials and TLS Secrets](#private-registry-configuration), then install:

```sh
kubectl -n flux-system create configmap bale-flux-releases --from-file=releases.yaml
helm upgrade resolver oci://mirror.example.com/release/charts/bale-flux:0.0.0-dev \
  --install --namespace flux-system --values values.yaml --wait
```

For a private registry CA, add Helm's `--ca-file registry-ca.crt` and [log in first](helm.md#private-registry-access). Plain Helm bootstraps the adapter using the destination image you supplied in values.

The example exposes `https://resolver-bale-flux.flux-system.svc:8080` with bearer authentication. Its certificate must cover that Service DNS name. The chart installs the Deployment and internal Service; install Flux Operator, providers and ResourceSets separately.

Files load at startup. Chart-managed allowlist changes trigger a rollout; restart the Deployment after changing an external ConfigMap or mounted Secret. `/healthz` checks the serving process without authentication or registry access, so registry failures do not restart it. See the [chart settings](https://github.com/strio-dev/bale/tree/main/charts/bale-flux) for all mounts and values.

## Connect Flux Operator

Copy and edit the [provider and ResourceSet example](https://github.com/strio-dev/bale/blob/main/plugins/helm-relocate/examples/flux/resources.yaml). Set the provider URL to `https://resolver-bale-flux.flux-system.svc:8080/inputs/web` and adapt its namespace, chart values and Secret names, then apply your file:

```sh
kubectl apply -f resources.yaml
```

The ResourceSet waits for the digest-pinned OCIRepository, then updates the stable HelmRelease's `chartRef` and image post-renderers together. Keep normal HelmRelease settings and values in that template. For custom deployment images, include them explicitly in the bundle and set their destination references in `HelmRelease.spec.values`.

Give each release one owning ResourceSet and a distinct release name in its namespace. Git manages the provider and ResourceSet; Flux Operator manages their generated resources. Do not manage those generated objects separately. Old sources are pruned after the final step is applied, before the HelmRelease becomes ready; the previous source is not retained until upgrade success.

`GET /inputs/web` resolves one configured release; `GET /inputs` resolves all atomically. For the aggregate endpoint, set the provider's `spec.filter.limit` to at least your release count: Flux Operator's default is 100, while the adapter permits 256.

Missing or ambiguous attachments, registry errors, timeouts and oversized responses fail the request. The adapter never returns a successful empty set or partial result. Flux Operator retains its last successful exported inputs on fetch failure, preserving the previous release inputs.

## Private registry configuration

Configure each connection independently:

| Client | Credentials | CA trust |
| --- | --- | --- |
| Adapter → registry | `registry.existingSecret` and `configKey` | `registry.ca.existingSecret` |
| Flux source-controller → registry | OCIRepository `secretRef` | OCIRepository `certSecretRef` |
| Kubelet → registry | Pod `imagePullSecrets` | Node container-runtime trust |
| Provider → adapter | Provider `secretRef` with `token` | Provider `certSecretRef` |

Provision `registry-config.json` with Docker-compatible `auths` entries for the exact destination host, including its port. The adapter image cannot run credential helpers: mount actual credentials scoped to the required repositories. Provision `registry-ca.crt` separately. Keep credentials, tokens and private keys outside bundles and Git.

For a fresh deployment using `flux-system` and `apps`:

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

Skip namespace creation if it already exists. The bootstrap values mount `.dockerconfigjson` and the CA into the adapter and set its own Pod's `imagePullSecrets`. The ResourceSet example configures the generated OCIRepository. Flux accepts a CA-only Secret through `certSecretRef`; supply client certificates too if your registry requires mutual TLS. See [Flux authentication](https://fluxcd.io/flux/components/source/ocirepositories/#secret-reference) and [TLS configuration](https://fluxcd.io/flux/components/source/ocirepositories/#mutual-tls-authentication).

Workload pull Secrets must exist in each Pod's namespace. Adapt the chart's values, including hook and subchart settings; Bale does not inject credentials. Install the registry CA in every relevant node's runtime trust configuration. Pod Secrets do not configure node trust. Use your cluster's node-management mechanism and the runtime's [registry configuration](https://github.com/containerd/containerd/blob/f2551031d7276a770f65f98c9b52e57e7dad07e8/docs/hosts.md#cri).

For provider → adapter HTTPS, provision `resolver.crt`, `resolver.key`, `resolver-ca.crt` and `resolver-token`. The certificate must cover `resolver-bale-flux.flux-system.svc`:

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

The provider uses the token and CA Secrets in `apps`; the adapter uses the matching token and server certificate in `flux-system`. These differ from registry credentials and CA trust. Provider `insecure: true` permits plain HTTP for local tests; it does not disable TLS verification.

## Run the binary directly

For an existing host deployment, build the native service and use the same allowlist:

```sh
nix build --no-update-lock-file path:.#flux-service --out-link result-flux-service
result-flux-service/bin/bale-flux --config releases.yaml \
  --listen 127.0.0.1:8080 --registry-config /run/secrets/registry/config.json
```

The registry config path must contain your provisioned credentials. For network access, set `--tls-cert-file`, `--tls-key-file` and `--bearer-token-file`, or terminate TLS at an ingress. Registry TLS uses separate `--ca-file`, `--cert-file` and `--key-file` options. Repeated `--plain-http HOST` permits HTTP for those exact registry hosts. Restart after config changes; resolution diagnostics go to stderr.

## Coverage and limits

The adapter emits exact Kustomize image rules for captured references, distinct tags, pinned digests and Docker Hub shorthand. Flux rewrites supported workload fields and hooks; valid unmapped images stay unchanged. Custom resource fields need a schema-aware consumer. Bale neither reads deployment values during capture nor verifies that every override image was included.

The integration targets Flux Operator **0.61.0**, helm-controller **1.6.5** and Kustomize API **0.21.1**. Exact matching depends on that Kustomize implementation; qualify controller upgrades against your chart profiles.

| Limit | Value |
| --- | --- |
| Configuration | 1 MiB; 256 releases |
| Chart metadata inspection | 256 advertised referrers; 4 MiB inventory and attachment reads |
| Serialized destination records | 4 MiB |
| Resolution | 30 seconds per request; 16 concurrent requests |
| Response | 900 KiB; no partial success |

Excess concurrent requests receive HTTP 503. These are metadata and request budgets, not process memory limits. The service reads attachments; it contains no chart renderer, container runtime, Wasm runtime or cluster API client.
