# Contributing

## Set up

Development requires Linux and Nix. Enter the pinned shell from a checkout:

```sh
nix develop path:.
bale --help
```

Direnv can load it with `direnv allow`. The shell provides Go, Just and linters; `bale` runs the current source.

## Build from source

```sh
just build
./result-bale/bin/bale --version
```

`path:.` includes untracked files. Git-based flake commands, including `just release-check`, use tracked files with their current working-tree edits.

## Check changes

Run focused checks while editing, then format and check the result:

```sh
just fmt
just check
```

| Command | Checks |
| --- | --- |
| `just test` | Tests in all Go modules. |
| `just vet` / `just lint` | Go static analysis. |
| `just scan` / `just vuln` | Security and dependency checks. |

Add regression coverage for behavior changes. Full checks on AMD64 require writable `/dev/kvm`; see [test suites](development/testing.md) for acceptance tests and prerequisites.

## Edit documentation

```sh
just docs-serve
just docs-build
```

Open <http://127.0.0.1:8000>. The strict build writes `result-docs/`. Add pages to `zensical.toml` and keep examples runnable. The pinned [Strio theme](https://github.com/strio-dev/strio-zensical-theme) supplies the shared styling and templates.

## Releases

Version tags publish the CLI archives, images, plugins and Flux chart. See [packaging and releases](development/releases.md) for their registry references and local builds.
