# Contributing

## Set up

Development requires Linux and Nix. From a checkout, enter the pinned shell:

```sh
nix develop path:.
bale --help
```

Direnv loads the same shell with `direnv allow`. The shell supplies Go, Just and linters; its `bale` command runs your current source. Read [how bundles work](concepts/bundles.md) before changing capture or delivery behavior.

## Build and check

```sh
just build
./result-bale/bin/bale --version
```

Use focused checks while editing, then run `just fmt` and `just check` before submitting:

| Command | Purpose |
| --- | --- |
| `just test` | Tests in all three Go modules. |
| `just vet` / `just lint` | Go static analysis in all modules. |
| `just fmt` | Format Go, Nix and the Justfile in place. |
| `just check` | Full flake checks, including documentation and plugin builds. |
| `just scan` / `just vuln` | Semgrep and Go vulnerability checks. |
| `just registry-acceptance` | Packaged CLI and consumers against native registries. |

Add regression coverage for behavior changes. See [test suites](development/testing.md) for plugin checks and optional live Kubernetes tests.

Nix commands using `path:.` include untracked files. `just release-check` checks the Git source, including working-tree edits to tracked files. Stage new files before that check.

## Edit documentation

```sh
just docs-serve
just docs-build
```

Open the preview at <http://127.0.0.1:8000>. The strict build writes `result-docs/`. Add pages to `zensical.toml`, keep examples runnable, and link detailed contracts from task guides.

The pinned [Strio theme](https://github.com/strio-dev/strio-zensical-theme) supplies styling, templates and copy controls. Bale owns its content, navigation, accent and logos.

## CI and releases

GitHub Actions runs quality, documentation, registry acceptance and native Linux AMD64/ARM64 checks. Semgrep and CodeQL publish security findings. Documentation deploys from `main` after its checks pass.

Build local CLI archives with `just release-build`. After checks pass, push an unprefixed version tag such as `1.2.3` or `1.2.3-rc.1`. CI publishes static Linux AMD64/ARM64 archives and `SHA256SUMS`, assembling assets in a draft first. Reruns leave published releases unchanged. Helm and Flux artifacts use their [separate packaging commands](development/testing.md#plugin-and-consumer-builds).
