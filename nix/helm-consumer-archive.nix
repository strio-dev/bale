{
  lib,
  stdenv,
  runCommand,
  kubernetes-helm,
  python3,
  helm-consumer,
}:
let
  pluginVersion = helm-consumer.pluginVersion;
  archiveName = "bale-${pluginVersion}.tgz";
  platform = "${stdenv.hostPlatform.go.GOOS}/${stdenv.hostPlatform.go.GOARCH}";
  platformTag = "${
    lib.replaceStrings [ "+" ] [ "_" ] pluginVersion
  }-${stdenv.hostPlatform.go.GOOS}-${stdenv.hostPlatform.go.GOARCH}";
  pluginDirectory = "${helm-consumer}/share/helm/plugins/bale";
in
runCommand "bale-helm-consumer-archive-${platformTag}"
  {
    nativeBuildInputs = [
      kubernetes-helm
      python3
    ];
    passthru = {
      inherit
        archiveName
        pluginVersion
        platform
        platformTag
        ;
    };
    meta = {
      description = "Native Bale plugin archive in Helm's distribution format";
      license = lib.licenses.mit;
      platforms = lib.platforms.linux;
    };
  }
  ''
    export HOME="$TMPDIR/home"
    export HELM_DATA_HOME="$TMPDIR/helm-data"
    export HELM_CONFIG_HOME="$TMPDIR/helm-config"
    export HELM_CACHE_HOME="$TMPDIR/helm-cache"
    export HELM_PLUGINS="$HELM_DATA_HOME/plugins"
    mkdir -p "$HOME" "$out" repeated

    # Immutable store inputs give Helm stable file timestamps and ownership.
    helm plugin package ${pluginDirectory} --sign=false --destination "$out"
    helm plugin package ${pluginDirectory} --sign=false --destination repeated
    cmp "$out/${archiveName}" repeated/${archiveName}

    helm plugin install "$out/${archiveName}" --verify=false
    helm plugin list --type cli/v1 > cli.list
    HELM_PLUGINS="$HELM_PLUGINS/bale/postrenderers" \
      helm plugin list --type postrenderer/v1 > renderer.list
    python3 ${../tests/packaging/helm_consumer_archive.py} \
      "$out/${archiveName}" ${pluginDirectory} "$HELM_PLUGINS/bale" \
      ${lib.escapeShellArg pluginVersion} ${stdenv.hostPlatform.go.GOARCH} cli.list renderer.list
  ''
