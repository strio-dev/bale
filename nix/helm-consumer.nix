{
  lib,
  stdenv,
  buildGoModule,
  go_1_27,
  version ? "dev",
  consumer ? "helm",
}:
assert lib.elem consumer [
  "helm"
  "flux"
];
let
  pluginVersion = if version == "dev" then "0.0.0" else lib.removePrefix "v" version;
  testSource = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../plugins/helm-relocate
      ../internal
      ../go.mod
      ../go.sum
      ../LICENSE
      (lib.fileset.unions (
        map (name: ../. + "/${name}") (
          builtins.filter (name: lib.hasSuffix ".go" name) (builtins.attrNames (builtins.readDir ../.))
        )
      ))
    ];
  };
in
(buildGoModule.override { go = go_1_27; }) (finalAttrs: {
  pname = if consumer == "helm" then "bale-helm-consumer" else "bale-flux-service";
  inherit version;
  passthru = { inherit pluginVersion testSource; };
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
            "examples"
          ]
        )
      );
  };
  overrideModAttrs = _: _: { src = testSource; };
  modRoot = "plugins/helm-relocate";
  vendorHash = "sha256-k3hpmaSKqoAmKhSz5IHd3IkyO5Ti0Fo7hr2JFcztKHc=";
  subPackages =
    if consumer == "helm" then
      [
        "cmd/bale-helm"
        "cmd/bale-relocate"
      ]
    else
      [ "cmd/bale-flux" ];
  env.CGO_ENABLED = "0";
  doCheck = false;
  ldflags = [
    "-s"
    "-w"
  ];
  postInstall =
    let
      vendorSource =
        if finalAttrs.vendorHash == null then "${finalAttrs.src}/vendor" else finalAttrs.goModules;
      notices = "$out/share/licenses/${finalAttrs.pname}/third-party";
    in
    (import ./go-notices.nix {
      go = go_1_27;
      inherit vendorSource;
      destination = notices;
    })
    + (
      if consumer == "helm" then
        ''
          pluginDir="$out/share/helm/plugins/bale"
          mkdir -p "$pluginDir"
          substitute helm/bale/plugin.yaml "$pluginDir/plugin.yaml" \
            --replace-fail 'version: 0.0.0' ${lib.escapeShellArg "version: ${pluginVersion}"} \
            --replace-fail '@os@' '${stdenv.hostPlatform.go.GOOS}' \
            --replace-fail '@arch@' '${stdenv.hostPlatform.go.GOARCH}'
          mv "$out/bin/bale-relocate" "$pluginDir/bale-relocate"
          mv "$out/bin/bale-helm" "$pluginDir/bale-helm"
          rendererDir="$pluginDir/postrenderers/bale-postrenderer"
          mkdir -p "$rendererDir"
          substitute helm/bale/postrenderers/bale-postrenderer/plugin.yaml "$rendererDir/plugin.yaml" \
            --replace-fail 'version: 0.0.0' ${lib.escapeShellArg "version: ${pluginVersion}"} \
            --replace-fail '@os@' '${stdenv.hostPlatform.go.GOOS}' \
            --replace-fail '@arch@' '${stdenv.hostPlatform.go.GOARCH}'
          rmdir "$out/bin"
          cp -R "${notices}" "$pluginDir/third-party"
          install -m644 ../../LICENSE "$pluginDir/LICENSE"
          install -Dm644 ../../LICENSE "$out/share/licenses/bale-helm-consumer/LICENSE"
        ''
      else
        ''
          install -Dm644 ../../LICENSE "$out/share/licenses/bale-flux-service/LICENSE"
        ''
    );
  meta = {
    description =
      if consumer == "helm" then
        "Native Helm integration for Bale image relocation"
      else
        "Flux Operator external inputs for Bale destination attachments";
    license = lib.licenses.mit;
    platforms = lib.platforms.linux;
  };
})
