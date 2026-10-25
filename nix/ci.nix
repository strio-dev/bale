{
  inputs,
  self,
  lib,
  ...
}:
let
  categories = {
    quality.x86_64-linux = lib.getAttrs [
      "go-check"
      "formatting"
      "justfile"
      "workflow"
      "release-publication"
      "helm-plugin-integration"
      "wasm-contract-fixture"
    ] self.checks.x86_64-linux;
    docs.x86_64-linux = lib.getAttrs [ "docs" "installer" ] self.checks.x86_64-linux;
    registry.x86_64-linux = lib.getAttrs [ "registry-acceptance" ] self.checks.x86_64-linux;
    integration.x86_64-linux = lib.getAttrs [ "container-acceptance" ] self.checks.x86_64-linux;
    native = lib.genAttrs [ "x86_64-linux" "aarch64-linux" ] (
      system:
      lib.getAttrs (
        [
          "build"
          "container"
          "release-binary"
          "helm-consumer"
          "helm-consumer-archive"
          "helm-plugin-image"
          "helm-plugin-archive"
          "helm-consumer-image"
          "distribution"
          "distribution-publish"
          "flux-service"
          "flux-service-image"
          "flux-service-chart"
        ]
        ++ lib.optionals (system == "aarch64-linux") [
          "go-check"
          "helm-plugin-integration"
        ]
      ) self.checks.${system}
    );
  };
  matrices = lib.mapAttrs (
    name: checks:
    let
      generated = inputs.nix-github-actions.lib.mkGithubMatrix {
        inherit checks;
        attrPrefix = "checks";
      };
    in
    {
      include = map (
        system:
        let
          rows = builtins.filter (row: row.system == system) generated.matrix.include;
        in
        {
          inherit name system;
          os = (builtins.head rows).os;
          attrs = map (row: row.attr) rows;
        }
      ) (builtins.attrNames checks);
    }
    // lib.optionalAttrs (name == "native") {
      system = builtins.attrNames checks;
    }
  ) categories;
in
{
  flake.githubActions = {
    inherit matrices;
    checks = lib.foldl' lib.recursiveUpdate { } (builtins.attrValues categories);
    matrix.include = lib.concatMap (matrix: matrix.include) (builtins.attrValues matrices);
  };
}
