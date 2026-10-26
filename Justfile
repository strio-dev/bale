set default-list

# Run tests in all Go modules with the pinned toolchain.
test:
    nix run --no-update-lock-file path:.#dev-test

# Run Go static analysis in all modules.
vet:
    nix run --no-update-lock-file path:.#dev-vet

# Run the pinned Go linter in all modules.
lint:
    nix run --no-update-lock-file path:.#dev-lint

# Format Go, Nix, and Justfile sources with pinned tools.
fmt:
    nix run --no-update-lock-file path:.#dev-fmt

# Run the pinned security rules.
scan:
    nix run --no-update-lock-file path:.#scan

# Check dependencies in all Go modules for known vulnerabilities.
vuln:
    nix run --no-update-lock-file path:.#dev-vuln

# Run the complete local flake check, including untracked sources.
check:
    nix flake check path:. --no-update-lock-file

# Build the Bale CLI.
build:
    nix build --no-update-lock-file path:.#bale --out-link result-bale

# Build the Bale CLI OCI image layout.
container-build:
    nix build --no-update-lock-file path:.#container --out-link result-container

# Build the strict documentation site.
docs-build:
    nix build --no-update-lock-file path:.#docs --out-link result-docs

# Preview documentation on localhost.
docs-serve:
    nix run --no-update-lock-file path:.#docs-preview

# Check the exact Git source that would be released.
release-check:
    nix flake check . --no-update-lock-file

# Build the CLI release archive.
release-build:
    nix build --no-update-lock-file path:.#release-binary --out-link result-release

# Build all distribution artifacts.
distribution-build:
    nix build --no-update-lock-file path:.#distribution --out-link result-distribution

# Build the Helm discovery WASI module.
helm-plugin-build:
    nix build --no-update-lock-file path:.#helm-plugin --out-link result-helm-plugin

# Package the discovery module, metadata, and license files.
helm-plugin-package:
    nix build --no-update-lock-file path:.#helm-plugin-archive --out-link result-helm-plugin-archive

# Check discovery code and its capture, transfer, and reuse flow.
helm-plugin-check:
    nix build --no-update-lock-file path:.#checks.{{ arch() }}-linux.helm-plugin-check path:.#checks.{{ arch() }}-linux.helm-plugin-integration --no-link

# Build the native Helm plugin directory.
helm-consumer-build:
    nix build --no-update-lock-file path:.#helm-consumer --out-link result-helm-consumer

# Package the installable native Helm plugin.
helm-consumer-package:
    nix build --no-update-lock-file path:.#helm-consumer-archive --out-link result-helm-consumer-archive

# Build the discovery plugin OCI layout.
helm-plugin-image-build:
    nix build --no-update-lock-file path:.#helm-plugin-image --out-link result-helm-plugin-image

# Build the native Helm plugin OCI layout.
helm-consumer-image-build:
    nix build --no-update-lock-file path:.#helm-consumer-image --out-link result-helm-consumer-image

# Build the Flux adapter executable.
flux-service-build:
    nix build --no-update-lock-file path:.#flux-service --out-link result-flux-service

# Build the Flux adapter OCI image layout.
flux-image-build:
    nix build --no-update-lock-file path:.#flux-service-image --out-link result-flux-service-image

# Package the Flux bootstrap chart.
flux-chart-build:
    nix build --no-update-lock-file path:.#flux-service-chart --out-link result-flux-service-chart

# Check packaged artifacts against Distribution and Zot.
registry-acceptance:
    nix build --no-update-lock-file path:.#checks.{{ arch() }}-linux.registry-acceptance --no-link

# Run rootless image acceptance in an AMD64 NixOS VM with KVM.
container-acceptance:
    nix build --no-update-lock-file path:.#checks.x86_64-linux.container-acceptance --no-link

# Check Helm and Flux Operator deployments in an AMD64 VM with KVM.
kubernetes-acceptance:
    nix run --no-update-lock-file path:.#kubernetes-acceptance
