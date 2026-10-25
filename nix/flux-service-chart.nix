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

    # Exercise the packaged chart, including SemVer build metadata and explicit
    # image overrides, so release tags render valid container references.
    mkdir version-fixture
    helm package chart --version 1.2.3+build.1 --app-version 1.2.3+build.1 \
      --destination version-fixture
    fixture=version-fixture/bale-flux-1.2.3+build.1.tgz
    helm template version-fixture "$fixture" --set existingConfigMap=chart-validation > default.yaml
    grep -Fq 'image: "ghcr.io/strio-dev/bale-flux:1.2.3_build.1"' default.yaml
    grep -Fq 'app.kubernetes.io/version: "1.2.3_build.1"' default.yaml
    helm template version-fixture "$fixture" --set existingConfigMap=chart-validation \
      --set-string image.tag=chosen-tag > explicit.yaml
    grep -Fq 'image: "ghcr.io/strio-dev/bale-flux:chosen-tag"' explicit.yaml
    digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    helm template version-fixture "$fixture" --set existingConfigMap=chart-validation \
      --set-string image.tag=chosen-tag --set-string image.digest="$digest" > digest.yaml
    grep -Fq "image: \"ghcr.io/strio-dev/bale-flux@$digest\"" digest.yaml
    helm show chart "$fixture" > metadata.yaml
    grep -Fxq 'version: 1.2.3+build.1' metadata.yaml
    grep -Fxq 'appVersion: 1.2.3+build.1' metadata.yaml
    grep -Fq 'org.opencontainers.image.source: https://github.com/strio-dev/bale' metadata.yaml
  ''
