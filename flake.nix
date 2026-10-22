{
  description = "Bale OCI container and artifact bundler";
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    flake-parts.inputs.nixpkgs-lib.follows = "nixpkgs";
    nix-github-actions.url = "github:nix-community/nix-github-actions";
    nix-github-actions.inputs.nixpkgs.follows = "nixpkgs";
    strio-theme.url = "github:strio-dev/strio-zensical-theme";
    strio-theme.inputs.nixpkgs.follows = "nixpkgs";
    strio-theme.inputs.flake-parts.follows = "flake-parts";
  };
  outputs =
    inputs:
    inputs.flake-parts.lib.mkFlake { inherit inputs; } {
      imports = [ ./nix/ci.nix ];
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      perSystem =
        {
          config,
          pkgs,
          system,
          ...
        }:
        let
          version =
            if builtins.pathExists ./nix/release-version then
              pkgs.lib.fileContents ./nix/release-version
            else
              "dev";
          go = pkgs.callPackage ./nix/go.nix { };
          bale = pkgs.callPackage ./nix/package.nix {
            inherit version;
            go_1_27 = go;
          };
          helm-plugin = pkgs.callPackage ./nix/helm-plugin.nix {
            inherit version;
            go_1_27 = go;
          };
          helm-consumer = pkgs.callPackage ./nix/helm-consumer.nix {
            inherit version;
            go_1_27 = go;
          };
          flux-service = pkgs.callPackage ./nix/helm-consumer.nix {
            inherit version;
            go_1_27 = go;
            consumer = "flux";
          };
          flux-service-image = pkgs.callPackage ./nix/flux-service-image.nix {
            inherit version flux-service;
          };
          flux-service-chart = pkgs.callPackage ./nix/flux-service-chart.nix { inherit version; };
          zot = pkgs.callPackage ./nix/zot.nix { };
          registry-acceptance = pkgs.callPackage ./nix/registry-acceptance.nix {
            inherit
              bale
              zot
              helm-plugin
              helm-consumer
              flux-service-image
              flux-service-chart
              ;
          };
          kubernetes-acceptance = pkgs.callPackage ./nix/kubernetes-acceptance.nix {
            inherit
              bale
              helm-plugin
              helm-consumer
              flux-service-image
              flux-service-chart
              ;
          };
          zensical = inputs.strio-theme.packages.${system}.zensical;
          docs = pkgs.callPackage ./nix/docs.nix {
            inherit system;
            inherit (inputs) strio-theme;
          };
          release = pkgs.callPackage ./nix/release-artifacts.nix { inherit pkgs bale; };
          release-binary = release.binary;
        in
        {
          packages = {
            default = bale;
            inherit
              bale
              docs
              release-binary
              helm-plugin
              helm-consumer
              flux-service
              flux-service-image
              flux-service-chart
              ;
          }
          // pkgs.lib.optionalAttrs (system == "x86_64-linux") {
            kubernetes-acceptance = kubernetes-acceptance.runner;
          };
          apps = {
            default = config.apps.bale;
            docs-preview = inputs.strio-theme.lib.mkPreview {
              inherit system;
              sourcePaths = [
                "docs"
                "zensical.toml"
              ];
            };
            bale = {
              type = "app";
              program = "${bale}/bin/bale";
            };
            registry-acceptance = {
              type = "app";
              program = "${registry-acceptance}/bin/bale-registry-acceptance";
            };
          }
          // pkgs.lib.optionalAttrs (system == "x86_64-linux") {
            kubernetes-acceptance = {
              type = "app";
              program = "${kubernetes-acceptance.runner}/bin/bale-kubernetes-acceptance";
            };
          };
          formatter = pkgs.nixfmt-tree;
          devShells.default = pkgs.callPackage ./nix/devshell.nix {
            inherit bale zensical;
            go_1_27 = go;
          };
          devShells.runtime = pkgs.mkShell {
            packages = [ go ];
            CGO_ENABLED = "0";
          };
          devShells.security = pkgs.callPackage ./nix/security.nix { };
          checks = import ./nix/checks.nix {
            inherit
              pkgs
              go
              bale
              docs
              helm-plugin
              helm-consumer
              flux-service
              flux-service-image
              flux-service-chart
              release
              ;
          };
        };
    };
}
