{
  lib,
  buildGoModule,
  go_1_27,
  version ? "dev",
}:
(buildGoModule.override { go = go_1_27; }) (finalAttrs: {
  pname = "bale";
  inherit version;
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../cmd
      (lib.fileset.unions (
        map (name: ../. + "/${name}") (
          builtins.filter (name: lib.hasSuffix ".go" name) (builtins.attrNames (builtins.readDir ../.))
        )
      ))
      ../internal
      ../plugin
      ../tests
      ../go.mod
      ../go.sum
      ../LICENSE
    ];
  };
  vendorHash = "sha256-ikLcKoSsS6+TSgiH2uHNF4NnFxfCr52x4mMQ8h02DDY=";
  subPackages = [ "cmd/bale" ];
  env.CGO_ENABLED = "0";
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];
  postInstall =
    let
      vendorSource =
        if finalAttrs.vendorHash == null then "${finalAttrs.src}/vendor" else finalAttrs.goModules;
    in
    ''
      install -Dm644 LICENSE "$out/share/licenses/bale/LICENSE"
      ${import ./go-notices.nix {
        go = go_1_27;
        inherit vendorSource;
        destination = "$out/share/licenses/bale/third-party";
      }}
    '';
  meta = {
    license = lib.licenses.mit;
    mainProgram = "bale";
  };
})
