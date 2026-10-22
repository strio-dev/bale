{
  lib,
  buildGoModule,
  go_1_27,
  golangci-lint,
  version ? "dev",
}:
(buildGoModule.override { go = go_1_27; }) (finalAttrs: {
  pname = "bale-helm-plugin";
  inherit version;
  src = lib.fileset.toSource {
    root = ../plugins/helm;
    fileset = lib.fileset.fileFilter (
      file:
      lib.elem file.name [
        "go.mod"
        "go.sum"
      ]
      || lib.any file.hasExt [
        "go"
        "json"
        "yaml"
        "yml"
        "tgz"
        "txt"
        "golden"
      ]
    ) ../plugins/helm;
  };
  vendorHash = "sha256-IB9t0wiBwEAXNpcmaZYMrUVL24I9GDqNYTvlFf8Y+Pw=";
  modPostBuild = ''
    patch --batch --fuzz=0 -d vendor/helm.sh/helm/v4 -p1 < ${./patches/helm-offline.patch}
  '';
  env.CGO_ENABLED = "0";
  nativeCheckInputs = [ golangci-lint ];
  buildPhase = ''
    runHook preBuild
    GOOS=wasip1 GOARCH=wasm go build -tags=helm_offline -ldflags="-s -w" -o helm.wasm ./cmd/bale-helm
    runHook postBuild
  '';
  checkPhase = ''
    runHook preCheck
    export GOLANGCI_LINT_CACHE="$TMPDIR/lint"
    go test ./...
    go test -tags=helm_offline ./...
    go vet ./...
    go vet -tags=helm_offline ./...
    golangci-lint run
    runHook postCheck
  '';
  installPhase = ''
    runHook preInstall
    install -Dm644 helm.wasm "$out/lib/bale/plugins/helm.wasm"
    install -Dm644 ${../LICENSE} "$out/share/licenses/bale-helm-plugin/LICENSE"
    runHook postInstall
  '';
  postInstall =
    let
      vendorSource =
        if finalAttrs.vendorHash == null then "${finalAttrs.src}/vendor" else finalAttrs.goModules;
    in
    (import ./go-notices.nix {
      go = go_1_27;
      inherit vendorSource;
      destination = "$out/share/licenses/bale-helm-plugin/third-party";
    })
    + ''
      install -Dm644 ${./patches/helm-offline.patch} "$out/share/licenses/bale-helm-plugin/third-party/helm-offline.patch"
      cat >> "$out/share/licenses/bale-helm-plugin/third-party/NOTICE" <<'NOTICE'
      Helm is modified for Bale offline rendering. The applied changes are
      included in helm-offline.patch; upstream Helm's Apache-2.0 license is
      retained in go-modules/helm.sh/helm/v4/LICENSE.
      NOTICE
    '';
  meta = {
    description = "WASI Helm dependency discovery plugin for Bale";
    license = lib.licenses.mit;
  };
})
