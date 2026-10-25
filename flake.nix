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
          development = pkgs.callPackage ./nix/development.nix { go_1_27 = go; };
          security = pkgs.callPackage ./nix/security.nix { };
          bale = pkgs.callPackage ./nix/package.nix {
            inherit version;
            go_1_27 = go;
          };
          container = pkgs.callPackage ./nix/container.nix { inherit bale; };
          release-commands = pkgs.callPackage ./nix/release-commands.nix { inherit container; };
          wasm-contract-fixture = pkgs.callPackage ./nix/wasm-contract-fixture.nix { };
          helm-plugin = pkgs.callPackage ./nix/helm-plugin.nix {
            inherit version;
            go_1_27 = go;
          };
          helm-plugin-archive = pkgs.callPackage ./nix/helm-plugin-archive.nix { inherit helm-plugin; };
          helm-consumer = pkgs.callPackage ./nix/helm-consumer.nix {
            inherit version;
            go_1_27 = go;
          };
          helm-consumer-archive = pkgs.callPackage ./nix/helm-consumer-archive.nix {
            inherit helm-consumer;
          };
          helm-plugin-image = pkgs.callPackage ./nix/helm-plugin-image.nix { inherit helm-plugin-archive; };
          helm-consumer-image = pkgs.callPackage ./nix/helm-consumer-image.nix {
            inherit helm-consumer-archive;
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
              integration-release
              publish
              helm-plugin-archive
              helm-consumer-archive
              flux-service-image
              flux-service-chart
              ;
          };
          kubernetes-acceptance = pkgs.callPackage ./nix/kubernetes-acceptance.nix {
            inherit
              bale
              helm-plugin-archive
              helm-consumer-archive
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
          distribution = pkgs.callPackage ./nix/distribution.nix {
            inherit
              version
              release-binary
              container
              helm-plugin
              helm-plugin-archive
              helm-plugin-image
              helm-consumer-archive
              helm-consumer-image
              flux-service-image
              flux-service-chart
              ;
          };
          publish = pkgs.callPackage ./nix/distribution-publish.nix {
            inherit
              container
              helm-plugin-image
              helm-consumer-image
              flux-service-image
              flux-service-chart
              ;
          };
          integration-release = pkgs.callPackage ./nix/integration-release.nix {
            inherit version publish helm-consumer-archive;
            container-index = release-commands.container-index;
          };
        in
        {
          packages = {
            default = bale;
            inherit
              bale
              container
              wasm-contract-fixture
              docs
              release-binary
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
          }
          // pkgs.lib.optionalAttrs (system == "x86_64-linux") {
            kubernetes-acceptance = kubernetes-acceptance.runner;
          };
          apps =
            pkgs.lib.mapAttrs' (
              name: command:
              pkgs.lib.nameValuePair "dev-${name}" {
                type = "app";
                program = "${command}/bin/bale-dev-${name}";
              }
            ) development.commands
            // {
              scan = {
                type = "app";
                program = "${security.scan}/bin/bale-scan";
              };
              publish = {
                type = "app";
                program = "${publish}/bin/bale-publish";
              };
            }
            // pkgs.lib.mapAttrs (name: command: {
              type = "app";
              program = "${command}/bin/bale-${name}";
            }) (pkgs.lib.filterAttrs (_: pkgs.lib.isDerivation) release-commands)
            // pkgs.lib.mapAttrs (name: command: {
              type = "app";
              program = "${command}/bin/bale-${name}";
            }) (pkgs.lib.filterAttrs (_: pkgs.lib.isDerivation) integration-release)
            // {
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
          devShells.security = security.shell;
          devShells.release = pkgs.mkShell {
            packages = [
              pkgs.skopeo
              pkgs.regctl
              pkgs.kubernetes-helm
              pkgs.gnutar
              pkgs.gzip
            ];
          };
          checks = import ./nix/checks.nix {
            inherit
              pkgs
              go
              bale
              container
              registry-acceptance
              wasm-contract-fixture
              docs
              helm-plugin
              helm-plugin-archive
              helm-consumer
              helm-consumer-archive
              helm-plugin-image
              helm-consumer-image
              distribution
              publish
              flux-service
              flux-service-image
              flux-service-chart
              release
              ;
          };
        };
    };
}
