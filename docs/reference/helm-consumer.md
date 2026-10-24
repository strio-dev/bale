# Helm consumer reference

The [Helm guide](../helm.md) covers capture, delivery and deployment. This reference describes how the native plugin selects destination records and rewrites rendered images.

## Command syntax

```text
helm bale [Bale selectors] install|upgrade|template RELEASE OCI_CHART [Helm flags]
```

The chart needs an explicit tag, digest or exact `--version`. Version ranges and implicit highest-version selection are unsupported. Helm receives the pinned chart and retains its environment, streams and exit status. Bale occupies the post-renderer slot.

## Select a capture

The consumer requires one matching local destination attachment and one selected dependency resolution:

| Selector | Value |
| --- | --- |
| `--bundle` | Delivered bundle repository and digest. |
| `--source` | Original chart repository and digest. |
| `--capture` | Owning capture digest. |
| `--declaration` | Selected declaration digest. |
| `--context` | Declaration context string. |

Place selectors before the action. Selection never chooses the newest attachment or combines captures. An explicitly selected empty declaration is valid. Missing, disabled or unselected dependency observations fail before Helm starts.

## Supported images and limits

The renderer handles the [workloads supported by discovery](helm-discovery.md#image-inventory), including hooks, init containers, ephemeral containers and nested Lists. It replaces mapped source selectors with pinned destination references and leaves valid unmapped references unchanged. Custom resource fields need a consumer that understands their schema.

Malformed workloads, invalid images and invalid metadata fail without partial YAML. Anchors, aliases and merge keys on paths requiring mutation are rejected.

| Input or output | Limit |
| --- | --- |
| Rendered YAML input | 8 MiB |
| Relocated YAML output | 16 MiB |
| Advertised referrers | 256 |
| Inventory and attachment reads | 4 MiB |
| Serialized destination records | 4 MiB |

The adapter passes records through a temporary `0600` file and removes it when Helm exits.

## Registry credentials

Chart retrieval and attachment lookup share `--plain-http`, `--ca-file`, `--cert-file`, `--key-file`, `--username`, `--password` and `--insecure-skip-tls-verify`. Prefer credential stores to passwords in arguments.

Helm 4.3.0's OCI plugin installer reads the default Helm credential location or Docker credentials. A custom `--registry-config` alone does not reach it; keep `HELM_CONFIG_HOME` and `HELM_REGISTRY_CONFIG` aligned as in the [login example](../helm.md#private-registry-access). This follows the [pinned client implementation](https://github.com/helm/helm/blob/bec5b06ed841fe5269972d864d5177944fd5970f/pkg/registry/client.go#L87-L119).

## Filter an existing stream

Inspect the exact delivered chart digest, then pass the records and rendered YAML to the installed filter:

```sh
chart_ref='mirror.example.com/release/charts/web@sha256:<full-chart-digest>'
bale destinations inspect "$chart_ref" > destinations.json
"$(helm env HELM_PLUGINS)/bale/bale-relocate" \
  --chart="$chart_ref" --records-file=destinations.json \
  < rendered.yaml > relocated.yaml
```

Substitute the full digest and supply `rendered.yaml`. The filter makes no registry requests. It cannot authenticate which chart produced an arbitrary stream: `--chart` is the caller's assertion.

`--records-file` accepts 4 MiB; `--records-json` accepts 64 KiB. The same capture selectors apply. This path also exposes renderer diagnostics that Helm may hide.
