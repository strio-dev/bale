# Bale Flux adapter chart

Deploys `bale-flux`, which supplies destination attachment mappings to Flux
Operator's ExternalService input provider. It does not install Flux Operator,
input providers, or ResourceSets.

Configure an allowlist in values:

```yaml
releases:
  - name: web
    chart: mirror.example.com/charts/web:1.0.0
    selection:
      context: production
```

Use `existingConfigMap` instead of `releases` to load a ConfigMap containing
`releases.yaml`. The configuration is read at startup: restart the Deployment
when changing an external ConfigMap or any mounted Secret. Chart-managed
configuration changes trigger a rollout automatically.

`image.digest` pins an image and takes precedence over `image.tag`, which defaults
to the chart appVersion. `imagePullSecrets` authenticate Kubernetes image pulls;
`registry.existingSecret` supplies separate Docker/Helm auth JSON for the adapter's
registry requests. Use credentials embedded in that JSON; the minimal image does
not include external credential helpers.

All credentials and certificates come from existing Secrets in the release
namespace. Their default keys and optional replacements are in [values.yaml](values.yaml):

- `bearerToken.existingSecret`: bearer token required by the HTTP service.
- `registry.existingSecret`: registry auth JSON.
- `registry.ca.existingSecret`: additional registry CA certificate.
- `registry.clientTLS.existingSecret`: registry client certificate and key.
- `serverTLS.existingSecret`: server certificate and key; enables HTTPS on the
  same Service port. Clients must trust this certificate and use its DNS name.

The Service is internal to the cluster. Configure the Flux Operator provider's
bearer token and CA separately. Plain HTTP registries require explicit host names
in `registry.plainHTTP`. HTTP(S) probes use the unauthenticated `/healthz` endpoint
without querying registries. The Pod has no Kubernetes API token or RBAC,
runs as UID/GID 65532, and has a read-only root with a small writable home volume.
