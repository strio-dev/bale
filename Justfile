set default-list

test:
    nix develop path:. -c go test ./...
    nix develop path:. -c sh -c 'cd plugins/helm && go test ./...'
    nix develop path:. -c sh -c 'cd plugins/helm-relocate && go test ./...'

helm-plugin-build:
    nix build --no-update-lock-file path:.#helm-plugin --out-link result-helm-plugin

helm-plugin-check:
    nix build --no-update-lock-file path:.#checks.{{ arch() }}-linux.helm-plugin-integration --no-link

helm-consumer-build:
    nix build --no-update-lock-file path:.#helm-consumer --out-link result-helm-consumer

flux-service-build:
    nix build --no-update-lock-file path:.#flux-service --out-link result-flux-service

flux-image-build:
    nix build --no-update-lock-file path:.#flux-service-image --out-link result-flux-service-image

flux-chart-build:
    nix build --no-update-lock-file path:.#flux-service-chart --out-link result-flux-service-chart

registry-acceptance:
    nix run --no-update-lock-file path:.#registry-acceptance

kubernetes-acceptance:
    nix run --no-update-lock-file path:.#kubernetes-acceptance

vet:
    nix develop path:. -c go vet ./...
    nix develop path:. -c sh -c 'cd plugins/helm && go vet ./...'
    nix develop path:. -c sh -c 'cd plugins/helm-relocate && go vet ./...'

lint:
    nix develop path:. -c golangci-lint run
    nix develop path:. -c sh -c 'cd plugins/helm && golangci-lint run'
    nix develop path:. -c sh -c 'cd plugins/helm-relocate && golangci-lint run'

fmt:
    nix develop path:. -c gofmt -w *.go cmd internal plugin tests
    nix develop path:. -c gofmt -w plugins/helm plugins/helm-relocate
    nix develop path:. -c nixfmt flake.nix nix/*.nix
    nix develop path:. -c just --fmt

check:
    nix flake check path:. --no-update-lock-file

build:
    nix build path:.#bale --out-link result-bale

release-check:
    nix flake check . --no-update-lock-file

release-build:
    nix build path:.#release-binary --out-link result-release

scan:
    nix develop path:.#security -c semgrep scan --error --strict --exclude 'charts/bale-flux/templates/**' .

vuln:
    nix develop path:. -c govulncheck ./...
    nix develop path:. -c sh -c 'cd plugins/helm && govulncheck ./...'
    nix develop path:. -c sh -c 'cd plugins/helm-relocate && govulncheck ./...'

docs-build:
    nix build --no-update-lock-file path:.#docs --out-link result-docs

docs-serve:
    nix run --no-update-lock-file path:.#docs-preview
