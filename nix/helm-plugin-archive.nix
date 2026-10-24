{
  lib,
  runCommand,
  gnutar,
  gzip,
  diffutils,
  helm-plugin,
}:
let
  pluginVersion =
    if helm-plugin.version == "dev" then "0.0.0" else lib.removePrefix "v" helm-plugin.version;
  archiveName = "helm-discovery-${pluginVersion}.tgz";
in
runCommand "bale-helm-discovery-archive-${pluginVersion}"
  {
    nativeBuildInputs = [
      gnutar
      gzip
      diffutils
    ];
    passthru = { inherit pluginVersion archiveName; };
    meta = {
      description = "Portable Bale discovery plugin package";
      license = lib.licenses.mit;
      platforms = lib.platforms.linux;
    };
  }
  ''
    mkdir helm-discovery "$out" unpacked
    cp -R ${helm-plugin}/share/licenses/bale-helm-plugin/. helm-discovery/
    cp ${helm-plugin}/lib/bale/plugins/helm.wasm helm-discovery/plugin.wasm
    substitute ${../plugins/helm/plugin.yaml} helm-discovery/plugin.yaml \
      --replace-fail 'version: 0.0.0' ${lib.escapeShellArg "version: ${pluginVersion}"}
    find helm-discovery -type f -exec chmod 0644 {} +
    find helm-discovery -type d -exec chmod 0755 {} +
    tar --sort=name --mtime=@1 --owner=0 --group=0 --numeric-owner \
      -cf - helm-discovery | gzip -n > "$out/${archiveName}"
    tar -xzf "$out/${archiveName}" -C unpacked
    diff -r helm-discovery unpacked/helm-discovery
  ''
