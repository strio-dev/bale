{
  pkgs,
  go,
  bale,
  container,
  docs,
  helm-plugin,
  helm-consumer,
  flux-service,
  flux-service-image,
  flux-service-chart,
  release,
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
  installer = pkgs.callPackage ./installer-check.nix { };
  go-check = quality;
  inherit
    helm-plugin
    helm-consumer
    flux-service
    flux-service-image
    flux-service-chart
    ;
  helm-plugin-integration = bale.overrideAttrs (old: {
    pname = "bale-helm-plugin-integration";
    env = old.env // {
      BALE_HELM_PLUGIN = "${helm-plugin}/lib/bale/plugins/helm.wasm";
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
        nixfmt --check ${../flake.nix} ${./.}/*.nix
        touch "$out"
      '';
  justfile = pkgs.runCommand "bale-justfile" { nativeBuildInputs = [ pkgs.just ]; } ''
    just --justfile ${../Justfile} --fmt --check
    touch "$out"
  '';
}
