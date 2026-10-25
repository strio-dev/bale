{
  pkgs,
  go,
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
  integration-release,
}:
let
  quality = bale.overrideAttrs {
    nativeCheckInputs = [ pkgs.golangci-lint ];
    checkPhase = ''
      export HOME="$TMPDIR"
      export GOLANGCI_LINT_CACHE="$TMPDIR/lint"
      go test ./...
      go vet ./...
      golangci-lint run
    '';
  };
in
{
  build = bale;
  inherit container;
  release-binary = release.check;
  release-publication =
    pkgs.runCommand "bale-release-publication-check" { nativeBuildInputs = [ pkgs.python3 ]; }
      ''
        python3 ${./tests/integration_release.py} \
          ${integration-release.integration-stage}/bin/bale-integration-stage \
          ${integration-release.integration-publish}/bin/bale-integration-publish
        touch "$out"
      '';
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
  go-check = quality;
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
  helm-plugin-integration = bale.overrideAttrs (old: {
    pname = "bale-helm-plugin-integration";
    env = old.env // {
      BALE_HELM_PLUGIN = "${helm-plugin}/lib/bale/plugins/helm.wasm";
      BALE_HELM_PLUGIN_ARCHIVE = "${helm-plugin-archive}/${helm-plugin-archive.archiveName}";
    };
    checkPhase = ''
      export HOME="$TMPDIR"
      go test ./tests/integration -run TestHelmPlugin -count=1 -timeout=5m
    '';
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
          pkgs.nixfmt
        ];
      }
      ''
        test -z "$(gofmt -l ${bale.src} ${helm-plugin.src} ${helm-consumer.src})"
        find ${../flake.nix} ${./.} -name '*.nix' -type f -print0 | xargs -0 nixfmt --check
        touch "$out"
      '';
  justfile = pkgs.runCommand "bale-justfile" { nativeBuildInputs = [ pkgs.just ]; } ''
    just --justfile ${../Justfile} --fmt --check
    touch "$out"
  '';
}
// pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isx86_64 {
  container-acceptance = pkgs.callPackage ./container-acceptance.nix {
    inherit container registry-acceptance;
  };
}
