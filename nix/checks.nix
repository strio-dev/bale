{
  pkgs,
  go,
  development,
  bale,
  container,
  registry-acceptance,
  wasm-contract-fixture,
  docs,
  helm-plugin,
  helm-plugin-archive,
  helm-consumer,
  helm-consumer-archive,
  helm-plugin-image,
  helm-consumer-image,
  distribution,
  publish,
  flux-service,
  flux-service-image,
  flux-service-chart,
  release,
}:
let
  goCheck =
    name: package: command: inputs:
    package.overrideAttrs {
      pname = "bale-${name}";
      src = package.testSource;
      goModules = package.goModules;
      doCheck = true;
      nativeCheckInputs = inputs;
      buildPhase = "runHook preBuild; runHook postBuild";
      checkPhase = ''
        runHook preCheck
        export HOME="$TMPDIR"
        export GOLANGCI_LINT_CACHE="$TMPDIR/lint"
        ${command}
        runHook postCheck
      '';
      installPhase = ''touch "$out"'';
      postInstall = "";
    };
in
{
  build = bale;
  inherit container;
  release-binary = release.check;
  distribution-publish = publish;
  wasm-contract-fixture = pkgs.runCommand "bale-wasm-contract-fixture-check" { } ''
    cmp ${wasm-contract-fixture}/lib/bale/rust-producer.wasm ${../internal/wasm/testdata/rust-producer.wasm}
    touch "$out"
  '';
  registry-acceptance = pkgs.runCommand "bale-registry-acceptance-check" { } ''
    ${registry-acceptance}/bin/bale-registry-acceptance
    touch "$out"
  '';
  installer = pkgs.callPackage ./installer-check.nix { };
  go-check = goCheck "go-check" bale ''
    go test ./...
    go vet ./...
    golangci-lint run
  '' [ pkgs.golangci-lint ];
  helm-plugin-check = goCheck "helm-plugin-check" helm-plugin ''
    go test ./...
    go test -tags=helm_offline ./...
    go vet ./...
    go vet -tags=helm_offline ./...
    golangci-lint run
  '' [ pkgs.golangci-lint ];
  helm-consumer-check = goCheck "helm-consumer-check" helm-consumer ''
    go test ./...
    go vet ./...
    golangci-lint run
  '' [ pkgs.golangci-lint ];
  inherit
    helm-plugin
    helm-plugin-archive
    helm-consumer
    helm-consumer-archive
    helm-plugin-image
    helm-consumer-image
    distribution
    flux-service
    flux-service-image
    flux-service-chart
    ;
  helm-plugin-integration =
    (goCheck "helm-plugin-integration" bale ''
      go test ./tests/integration -run TestHelmPlugin -count=1 -timeout=5m
    '' [ ]).overrideAttrs
      (old: {
        env = old.env // {
          BALE_HELM_PLUGIN = "${helm-plugin}/lib/bale/plugins/helm.wasm";
          BALE_HELM_PLUGIN_ARCHIVE = "${helm-plugin-archive}/${helm-plugin-archive.archiveName}";
        };
      });
  inherit docs;
  workflow =
    pkgs.runCommand "bale-workflow"
      {
        nativeBuildInputs = [
          pkgs.actionlint
          pkgs.shellcheck
        ];
      }
      ''
        # GitHub supports queue:max; pinned actionlint predates that schema key.
        actionlint -ignore 'unexpected key "queue" for "concurrency" section' ${../.github/workflows}/*.yml
        touch "$out"
      '';
  formatting =
    pkgs.runCommand "bale-formatting"
      {
        nativeBuildInputs = [
          go
          pkgs.findutils
          pkgs.nixfmt
          pkgs.just
        ];
      }
      ''
        cd ${development.formatSource}
        set -- --check
        ${development.format}
        touch "$out"
      '';
}
// pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isx86_64 {
  container-acceptance = pkgs.callPackage ./container-acceptance.nix {
    inherit container registry-acceptance;
  };
}
