{
  lib,
  stdenvNoCC,
  fetchurl,
  autoPatchelfHook,
  glibc,
}:
let
  assets = {
    x86_64-linux = {
      arch = "amd64";
      hash = "sha256-oFMWn5e0zqoFHgVNU5cXmRiKOYyzasuvXghiLshmTEQ=";
    };
    aarch64-linux = {
      arch = "arm64";
      hash = "sha256-QiTCMSLENjj5KHXW+08jVkRy0lhKNIywV0QLz0tDvwg=";
    };
  };
  asset = assets.${stdenvNoCC.hostPlatform.system};
in
stdenvNoCC.mkDerivation (finalAttrs: {
  pname = "zot-minimal";
  version = "2.1.22";
  src = fetchurl {
    url = "https://github.com/project-zot/zot/releases/download/v${finalAttrs.version}/zot-linux-${asset.arch}-minimal";
    inherit (asset) hash;
  };
  dontUnpack = true;
  dontStrip = true;
  nativeBuildInputs = [ autoPatchelfHook ];
  buildInputs = [ glibc ];
  installPhase = ''
    runHook preInstall
    install -Dm755 "$src" "$out/bin/zot"
    runHook postInstall
  '';
  meta = {
    description = "Minimal Zot registry for local acceptance tests";
    homepage = "https://zotregistry.dev";
    license = lib.licenses.asl20;
    mainProgram = "zot";
    platforms = builtins.attrNames assets;
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
  };
})
