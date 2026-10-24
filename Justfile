set default-list

test:
    nix run --no-update-lock-file path:.#dev-test

helm-plugin-build:
    nix build --no-update-lock-file path:.#helm-plugin --out-link result-helm-plugin

helm-plugin-package:
    nix build --no-update-lock-file path:.#helm-plugin-archive --out-link result-helm-plugin-archive

helm-plugin-check:
    nix build --no-update-lock-file path:.#checks.{{ arch() }}-linux.helm-plugin-integration --no-link

helm-consumer-build:
    nix build --no-update-lock-file path:.#helm-consumer --out-link result-helm-consumer

helm-consumer-package:
    nix build --no-update-lock-file path:.#helm-consumer-archive --out-link result-helm-consumer-archive

helm-plugin-image-build:
    nix build --no-update-lock-file path:.#helm-plugin-image --out-link result-helm-plugin-image

helm-consumer-image-build:
    nix build --no-update-lock-file path:.#helm-consumer-image --out-link result-helm-consumer-image

distribution-build:
    nix build --no-update-lock-file path:.#distribution --out-link result-distribution

flux-service-build:
    nix build --no-update-lock-file path:.#flux-service --out-link result-flux-service

flux-image-build:
    nix build --no-update-lock-file path:.#flux-service-image --out-link result-flux-service-image

flux-chart-build:
    nix build --no-update-lock-file path:.#flux-service-chart --out-link result-flux-service-chart

registry-acceptance:
    nix build --no-update-lock-file path:.#checks.{{ arch() }}-linux.registry-acceptance --no-link

kubernetes-acceptance:
    nix run --no-update-lock-file path:.#kubernetes-acceptance

vet:
    nix run --no-update-lock-file path:.#dev-vet

lint:
    nix run --no-update-lock-file path:.#dev-lint

fmt:
    nix run --no-update-lock-file path:.#dev-fmt

check:
    nix flake check path:. --no-update-lock-file

build:
    nix build --no-update-lock-file path:.#bale --out-link result-bale

container-acceptance:
    nix build --no-update-lock-file path:.#checks.x86_64-linux.container-acceptance --no-link

container-build:
    nix build --no-update-lock-file path:.#container --out-link result-container

release-check:
    nix flake check . --no-update-lock-file

release-build:
    nix build --no-update-lock-file path:.#release-binary --out-link result-release

scan:
    nix run --no-update-lock-file path:.#scan

vuln:
    nix run --no-update-lock-file path:.#dev-vuln

docs-build:
    nix build --no-update-lock-file path:.#docs --out-link result-docs

docs-serve:
    nix run --no-update-lock-file path:.#docs-preview
