# Flux

`bale-flux` reads destination attachments and serves chart inputs to [Flux Operator's ExternalService provider](https://fluxoperator.dev/docs/crd/resourcesetinputprovider/#type). A ResourceSet creates ordinary digest-pinned OCIRepository and HelmRelease resources. Charts stay unchanged, and Bale push runs no integration code. This requires Flux Operator in addition to Flux; stock Flux does not discover Bale attachments.

## Package and install

Build the OCI image layout and Helm chart from the checkout:

```sh
just flux-image-build
just flux-chart-build
```

Nix packages the static service binary, CA certificates and license without running containers. Skopeo converts the locally built archive into a standard OCI image layout. The image runs as UID/GID 65532 and contains no shell, credential helpers or Nix store closure. Each native build produces one Linux platform image; the flake offers AMD64 and ARM64 targets.

Publish development artifacts to your source registry with Skopeo and Helm:

```sh
skopeo copy oci:result-flux-service-image:dev \
  docker://registry.example.com/images/bale-flux:dev
helm push result-flux-service-chart/bale-flux-0.0.0-dev.tgz \
  oci://registry.example.com/charts
```

Replace the source registry names in the [example recipe](examples/flux-bootstrap/bale.yaml). The recipe includes the chart and service image explicitly; bootstrap needs no discovery plugin or native Helm integration. Deployment values stay separate from capture.

```sh
bale create --layout -f bale.yaml bootstrap --output-delivery bale.delivery.yaml
```

In the generated delivery file, set the chart path to `charts/bale-flux` and the service image path to `images/bale-flux`. Then deliver to the chosen registry:

```sh
bale push bootstrap mirror.example.com/release:bootstrap --delivery bale.delivery.yaml
```

After delivery, write [installation values](examples/flux-bootstrap/values.yaml) with `image.repository: mirror.example.com/release/images/bale-flux` and `image.tag: dev`, adapting the destination to your push. Bale does not generate these values. Create the `flux-system` namespace and referenced Secrets, then a ConfigMap containing your destination release allowlist:

```sh
kubectl -n flux-system create configmap bale-flux-releases --from-file=releases.yaml
helm upgrade resolver oci://mirror.example.com/release/charts/bale-flux:0.0.0-dev \
  --install --namespace flux-system --values values.yaml --wait
```

Plain Helm uses the destination image from `values.yaml`, bootstrapping the resolver without contacting the source registry. The chart mounts its allowlist and existing Secrets, provides a cluster-internal Service, and runs with a read-only root and writable home. It installs no Flux Operator, provider, ResourceSet or RBAC.

With release name `resolver`, the Service is `resolver-bale-flux.flux-system.svc:8080`. The example values enable HTTPS and bearer authentication. The TLS certificate must cover that Service DNS name; configure the provider URL, token and CA accordingly. Kubernetes image-pull Secrets (`imagePullSecrets`) and resolver registry credentials (`registry.existingSecret`) are separate. The image cannot invoke external credential helpers; provide auth entries in its mounted JSON.

The service reads files at startup. Chart-managed allowlist changes trigger a rollout; restart the Deployment after updating an external ConfigMap or any mounted Secret. See the [chart values reference](https://github.com/strio-dev/bale/tree/main/charts/bale-flux) for all mounts and settings.

Readiness and liveness probes use `/healthz` over HTTP or HTTPS to match the listener. This endpoint needs no bearer token and performs no registry requests; registry failures do not restart a healthy resolver.

## Private registry configuration

For a destination such as `mirror.example.com` with a private CA, configure each client that connects to it:

| Client | Credentials | Registry CA trust |
| --- | --- | --- |
| `helm bale` | Helm registry configuration | `--ca-file` on each chart operation |
| Resolver | `registry.existingSecret` and `configKey` | `registry.ca.existingSecret` |
| Flux source-controller | OCIRepository `secretRef` | OCIRepository `certSecretRef` |
| Kubelet/container runtime | Pod `imagePullSecrets` | Node container-runtime configuration |

Start with a Docker-compatible `registry-config.json` containing `auths` entries for the destination host, including its port if present, and the PEM CA file `registry-ca.crt`. The JSON mounted in Kubernetes must contain credentials directly; helper-only configurations cannot run inside the resolver image. Use a registry account scoped to the required repositories. Keep these files and Secrets outside the bundle and Git.

Create the credentials in both namespaces used by this example. The same Secret can serve the resolver, Flux and Pod pulls when they use the same registry account:

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

Skip namespace creation if it already exists. The [bootstrap values](examples/flux-bootstrap/values.yaml) select `.dockerconfigjson` as the resolver's mounted config key, mount the registry CA, and set its own Pod's `imagePullSecrets`. The [ResourceSet example](https://github.com/strio-dev/bale/blob/main/plugins/helm-relocate/examples/flux/resources.yaml) sets the generated OCIRepository's credential and CA references. Flux accepts a CA-only Secret under `certSecretRef`; client certificates are needed only when the registry requires mutual TLS. See [Flux registry authentication](https://fluxcd.io/flux/components/source/ocirepositories/#secret-reference) and [TLS configuration](https://fluxcd.io/flux/components/source/ocirepositories/#mutual-tls-authentication).

Workload pull Secrets must exist in the Pod's namespace. Adapt `HelmRelease.spec.values.imagePullSecrets` to the actual chart: hook Jobs and subcharts may expose separate settings. Bale relocates image references; it does not inject pull credentials. See [Kubernetes private image pulls](https://kubernetes.io/docs/tasks/configure-pod-container/pull-image-private-registry/).

Install `registry-ca.crt` in the container runtime's trust configuration on every node that may run these Pods. A Pod Secret does not configure node trust. For containerd 2.x, enable `config_path = "/etc/containerd/certs.d"` under `[plugins."io.containerd.cri.v1.images".registry]`, then provision `/etc/containerd/certs.d/mirror.example.com/hosts.toml`:

```toml
server = "https://mirror.example.com"

[host."https://mirror.example.com"]
  capabilities = ["pull", "resolve"]
  ca = "/etc/containerd/certs.d/mirror.example.com/ca.crt"
```

Copy the registry CA to that `ca` path and include the registry port in the directory and URLs when applicable. Apply this through your cluster's node configuration; changing containerd's main configuration may require a restart. The [containerd 2.4.1 registry guide](https://github.com/containerd/containerd/blob/f2551031d7276a770f65f98c9b52e57e7dad07e8/docs/hosts.md#cri) describes the runtime settings.

Resolver HTTPS is a separate connection. With the bootstrap Service URL `https://resolver-bale-flux.flux-system.svc:8080/inputs/web`, create its server certificate Secret, matching bearer token Secrets, and provider CA Secret from locally provisioned files:

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

The server certificate must cover `resolver-bale-flux.flux-system.svc`. Set that URL in the provider example; its `secretRef` and `certSecretRef` use the token and CA in `apps`. These are independent of the registry credentials and registry CA. Finally, install the bootstrap chart with the [Helm credential and CA options](helm.md#private-registry-access), then apply the provider and ResourceSet.

## Run the binary directly

Build the native service:

```sh
nix build --no-update-lock-file path:.#flux-service --out-link result-flux-service
```

Write `releases.yaml` with the delivered charts the service may resolve:

```yaml
releases:
  - name: web
    chart: mirror.example.com/release/charts/web:1.4
    selection:
      context: production
```

Start the service:

```sh
result-flux-service/bin/bale-flux --config releases.yaml \
  --listen 127.0.0.1:8080 \
  --registry-config /run/secrets/registry/config.json
```

`GET /inputs/web` resolves one configured release; `GET /inputs` resolves all configured releases atomically. Requests cannot override the allowlist or selectors. Tags are resolved once per request. Selection uses the same rules as the [Helm integration](helm.md#selection-and-validation): one local attachment and one captured dependency resolution. Add `bundle`, `source`, `capture`, `declaration` or `context` under `selection` when necessary. There is no newest-record policy or capture union.

For the aggregate `/inputs` endpoint, set the provider's `spec.filter.limit` to at least the configured release count (up to 256). Flux Operator otherwise truncates to its default limit of 100. The single-release example uses `/inputs/web`.

For network access, configure `--tls-cert-file`, `--tls-key-file` and `--bearer-token-file`, or terminate TLS at your ingress. Registry trust uses separate `--ca-file`, `--cert-file` and `--key-file` options. Repeated `--plain-http HOST` permits HTTP only for those registry hosts. Configuration is loaded at startup; restart the service after changing it. Resolution failures return non-success HTTP responses and write detailed diagnostics to stderr.

## Connect Flux Operator

The complete [provider and ResourceSet example](https://github.com/strio-dev/bale/tree/main/plugins/helm-relocate/examples/flux) has two steps:

1. Create an OCIRepository named from the release identity, chart repository and digest, then wait until it is ready.
2. Update the stable HelmRelease's `chartRef` and image post-renderers together.

Keep chart values and normal HelmRelease settings in the ResourceSet template. Git owns the provider and ResourceSet; Flux Operator owns their generated resources. Give each release one owning ResourceSet and a distinct release name in its namespace. Do not separately manage generated objects from Git or share their ownership across ResourceSets. Old sources are pruned after the final step is applied, before the final HelmRelease becomes ready. This does not retain the previous source until an upgrade succeeds. A source changes identity when its release, repository or digest changes; a different capture of the same chart updates the post-renderers on the existing release.

Configure credentials separately for the provider's HTTP call, the resolver's registry access, and Flux source-controller's chart retrieval. Workload image-pull credentials remain a cluster concern. The provider Secret uses a `token` entry matching the resolver token file. Its `certSecretRef` can configure a private CA. Provider `insecure: true` permits plain HTTP for local tests; it does not disable TLS certificate verification.

Missing attachments, ambiguous selection, registry failures, timeouts and oversized responses fail the entire request. The service never returns a successful empty input set. Flux Operator retains its last successful exported inputs on fetch failure, so a metadata failure does not ask it to prune the previous release.

## Coverage and limits

The adapter emits native Kustomize image rules for exact captured references, including distinct tags in one repository, pinned digests and Docker Hub shorthand aliases. Flux's renderer handles the supported workload image fields and hooks. As with the native Helm filter, **unmapped images remain unchanged**.

For images introduced by custom deployment values, add ordinary recipe `include` entries before capture and set `HelmRelease.spec.values` to their transferred locations after delivery. No extra chart attachment is needed for these overrides. Discovery can use the chart defaults. Bale does not read deployment values, generate destination values or verify that every override was captured. Other custom resource fields need a consumer that understands their schema.

The integration targets Flux Operator **0.61.0**, helm-controller **1.6.5**, and its Kustomize API **0.21.1**. Exact reference matching relies on that Kustomize implementation's regular expression behavior. Qualify controller upgrades against your chart profiles.

Configuration is limited to 1 MiB and 256 named releases. Each chart inspection permits at most 256 advertised referrers and 4 MiB of inventory and attachment reads, including validation rereads. Its serialized records have a separate 4 MiB limit. These are metadata budgets, not process memory limits. Each request has a 30-second resolution timeout, a 900 KiB response limit, and no partial successful result. At most 16 requests resolve concurrently; excess requests receive HTTP 503. There is no chart renderer, container runtime, Wasm runtime, or cluster API client in the service.

The optional [Kubernetes acceptance command](contributing.md#kubernetes-acceptance) exercises the packaged service and real Flux Operator against an authenticated HTTPS registry with a private CA. It checks image identity, A→B→A, credential failure and recovery, cached-image pull authentication, TLS/token handling and ambiguous-attachment preservation.
