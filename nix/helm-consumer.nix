{
  lib,
  stdenv,
  buildGoModule,
  go_1_27,
  golangci-lint,
  version ? "dev",
  consumer ? "helm",
}:
assert lib.elem consumer [
  "helm"
  "flux"
];
(buildGoModule.override { go = go_1_27; }) (finalAttrs: {
  pname = if consumer == "helm" then "bale-helm-consumer" else "bale-flux-service";
  inherit version;
  src = lib.fileset.toSource {
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
  modRoot = "plugins/helm-relocate";
  vendorHash = "sha256-Xm/PsFj5TLfXRTDe7Ei18OINRPfUmXOfhBA62d2oxLk=";
  subPackages =
    if consumer == "helm" then
      [
        "cmd/bale-helm"
        "cmd/bale-relocate"
      ]
    else
      [ "cmd/bale-flux" ];
  env.CGO_ENABLED = "0";
  ldflags = [
    "-s"
    "-w"
  ];
  nativeCheckInputs = [ golangci-lint ];
  checkPhase = ''
    runHook preCheck
    export GOLANGCI_LINT_CACHE="$TMPDIR/lint"
    go test ./...
    go vet ./...
    golangci-lint run
    runHook postCheck
  '';
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
            --replace-fail '@os@' '${stdenv.hostPlatform.go.GOOS}' \
            --replace-fail '@arch@' '${stdenv.hostPlatform.go.GOARCH}'
          mv "$out/bin/bale-relocate" "$pluginDir/bale-relocate"
          mv "$out/bin/bale-helm" "$pluginDir/bale-helm"
          rendererDir="$pluginDir/postrenderers/bale-postrenderer"
          mkdir -p "$rendererDir"
          substitute helm/bale/postrenderers/bale-postrenderer/plugin.yaml "$rendererDir/plugin.yaml" \
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
