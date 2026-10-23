{
  lib,
  runCommand,
  kubernetes-helm,
  version ? "dev",
}:
let
  chartVersion = if version == "dev" then "0.0.0-dev" else lib.removePrefix "v" version;
in
runCommand "bale-flux-chart-${chartVersion}"
  {
    nativeBuildInputs = [ kubernetes-helm ];
    passthru = { inherit chartVersion; };
    meta = {
      description = "Helm chart for the Bale Flux adapter";
      license = lib.licenses.mit;
      platforms = lib.platforms.linux;
    };
  }
  ''
    export HOME="$TMPDIR"
    cp -R ${../charts/bale-flux} chart
    chmod -R u+w chart
    cp ${../LICENSE} chart/LICENSE
    find chart -exec touch -h -d @1 {} +
    helm lint --strict chart --set existingConfigMap=chart-validation
    helm template validation chart --set existingConfigMap=chart-validation > rendered.yaml
    mkdir -p "$out"
    helm package chart --version ${lib.escapeShellArg chartVersion} \
      --app-version ${lib.escapeShellArg version} --destination "$out"
  ''
