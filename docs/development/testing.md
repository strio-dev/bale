# Test suites

Use focused checks while editing, then run `just check`. The test, vet and lint commands cover the root library, discovery producer and native consumers.

Production builds compile and package artifacts. Separate flake checks run each Go module's tests, vet and lint on AMD64 and ARM64. Test-only edits leave production build inputs unchanged.

Root module tests also run with Go's race detector on both architectures. Nix supplies the required C compiler; production binaries remain statically built.

## Choose a check

| Command | Scope | Requirements |
| --- | --- | --- |
| `just test` | Go tests in all three modules. | Linux and Nix. |
| `just test-race` | Root module tests with the race detector. | Linux and Nix. |
| `just helm-plugin-check` | Discovery code checks and capture, transfer and reuse. | Linux and Nix. |
| `just registry-acceptance` | Packaged CLI and consumers against Distribution and Zot. | Linux and Nix. |
| `just container-acceptance` | Packaged image in rootless Podman. | Linux AMD64 and writable `/dev/kvm`. |
| `just kubernetes-acceptance` | Helm and Flux Operator deployments. | Linux AMD64 and writable `/dev/kvm`. |

Nix supplies the tools and registries. VM checks need neither a host container runtime nor an existing cluster. All checks above except Kubernetes acceptance are included in `just check`; ARM64 checks omit the container VM.

## Registry and container acceptance

Registry acceptance covers capture, refresh, offline restoration, delivery and destination-only retrieval. It exercises discovery packages, native Helm installation, publication retries, credentials and TLS trust against both registries.

Container acceptance runs the packaged image with its default user and an arbitrary user inside an isolated NixOS VM. It covers the same transfer lifecycle, credentials and private-CA trust. Test failures and missing prerequisites fail the selected suite.

## Kubernetes acceptance

The optional suite deploys unchanged charts through native Helm and Flux Operator, then checks running image digests across upgrades and rollback. It also exercises image overrides, private registry access and failure cases.

Retain cluster objects, logs and JUnit output in a new directory:

```sh
nix run --no-update-lock-file path:.#kubernetes-acceptance -- \
  --output result-kubernetes-acceptance
```

Results are under `evidence/`, with `junit.xml` alongside it. Without `--output`, results are temporary. The VM is cleaned up in either case.

The suite uses four CPUs, 4 GiB RAM and a sparse 16 GiB disk. It stays outside default CI; manual CI enables it with the `kubernetes` input. Tool and image pins live in `nix/kubernetes-inputs.nix`.

## CI

GitHub Actions runs quality, documentation, registry and container checks, plus native AMD64 and ARM64 builds. `nix/ci.nix` assigns checks to the generated matrices. Semgrep and CodeQL publish security findings separately; documentation deploys from `main` after its checks pass.

For build targets and publication commands, see [packaging and releases](releases.md).
