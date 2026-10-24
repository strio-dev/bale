{
  lib,
  stdenv,
  runCommand,
  version,
  release-binary,
  container,
  helm-plugin,
  helm-plugin-archive,
  helm-plugin-image,
  helm-consumer-archive,
  helm-consumer-image,
  flux-service-image,
  flux-service-chart,
}:
let
  platform = "${stdenv.hostPlatform.go.GOOS}-${stdenv.hostPlatform.go.GOARCH}";
in
runCommand "bale-distribution-${version}-${platform}"
  {
    meta = {
      description = "Bale release archives and OCI layouts for ${platform}";
      license = lib.licenses.mit;
      platforms = lib.platforms.linux;
    };
  }
  ''
    mkdir -p "$out/archives/helm-consumer-${platform}" "$out/oci" "$out/helm-discovery"
    cp ${release-binary}/*.tar.gz "$out/archives/"
    cp ${helm-consumer-archive}/${helm-consumer-archive.archiveName} "$out/archives/helm-consumer-${platform}/"
    cp ${flux-service-chart}/*.tgz "$out/archives/"
    cp ${helm-plugin-archive}/${helm-plugin-archive.archiveName} "$out/archives/"
    cp ${helm-plugin}/lib/bale/plugins/helm.wasm "$out/helm-discovery/"
    cp -R ${helm-plugin}/share/licenses/bale-helm-plugin "$out/helm-discovery/licenses"
    cp -R ${container} "$out/oci/bale"
    cp -R ${helm-plugin-image} "$out/oci/helm-discovery"
    cp -R ${helm-consumer-image} "$out/oci/helm-consumer"
    cp -R ${flux-service-image} "$out/oci/bale-flux"
  ''
