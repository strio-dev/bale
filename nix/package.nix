{
  lib,
  buildGoModule,
  go_1_27,
  version ? "dev",
}:
let
  testSource = lib.fileset.toSource {
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
      ../store
      ../tests
      ../go.mod
      ../go.sum
      ../LICENSE
    ];
  };
in
(buildGoModule.override { go = go_1_27; }) (finalAttrs: {
  pname = "bale";
  inherit version;
  src = lib.cleanSourceWith {
    src = testSource;
    filter =
      path: type:
      let
        name = builtins.baseNameOf path;
      in
      !(
        lib.hasSuffix "_test.go" name
        || (
          type == "directory"
          && lib.elem name [
            "testdata"
            "tests"
          ]
        )
      );
  };
  passthru.testSource = testSource;
  # Keep test imports in the pinned vendor tree used by dedicated checks.
  overrideModAttrs = _: _: { src = testSource; };
  vendorHash = "sha256-ikLcKoSsS6+TSgiH2uHNF4NnFxfCr52x4mMQ8h02DDY=";
  subPackages = [ "cmd/bale" ];
  doCheck = false;
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
