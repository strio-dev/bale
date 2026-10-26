{
  bale,
  helm-plugin-archive,
  helm-consumer-archive,
  flux-service-image,
  flux-service-chart,
  integration-release,
  publish,
  regctl,
  kubernetes-helm,
  distribution,
  zot,
  writeShellApplication,
  bale-cli ? "${bale}/bin/bale",
}:
let
  tests = bale.overrideAttrs {
    pname = "bale-registry-acceptance-tests";
    src = bale.testSource;
    goModules = bale.goModules;
    doCheck = false;
    buildPhase = ''
      runHook preBuild
      go test -c -o registry-acceptance.test ./tests/acceptance
      runHook postBuild
    '';
    installPhase = ''
      runHook preInstall
      install -Dm755 registry-acceptance.test "$out/bin/registry-acceptance.test"
      runHook postInstall
    '';
    postInstall = "";
  };
in
writeShellApplication {
  name = "bale-registry-acceptance";
  text = ''
    export BALE_ACCEPTANCE_BINARY=${bale-cli}
    export BALE_ACCEPTANCE_DISTRIBUTION=${distribution}/bin/registry
    export BALE_ACCEPTANCE_ZOT=${zot}/bin/zot
    export BALE_ACCEPTANCE_HELM_PLUGIN=${helm-plugin-archive}/${helm-plugin-archive.archiveName}
    export BALE_ACCEPTANCE_HELM=${kubernetes-helm}/bin/helm
    export BALE_ACCEPTANCE_HELM_CONSUMER_ARCHIVE=${helm-consumer-archive}/${helm-consumer-archive.archiveName}
    export BALE_ACCEPTANCE_FLUX_IMAGE=${flux-service-image}
    export BALE_ACCEPTANCE_FLUX_CHART=${flux-service-chart}/bale-flux-${flux-service-chart.chartVersion}.tgz
    export BALE_ACCEPTANCE_INTEGRATION_STAGE=${integration-release.integration-stage}/bin/bale-integration-stage
    export BALE_ACCEPTANCE_INTEGRATION_PUBLISH=${integration-release.integration-publish}/bin/bale-integration-publish
    export BALE_ACCEPTANCE_REGCTL=${regctl}/bin/regctl
    export BALE_ACCEPTANCE_PUBLISH=${publish}/bin/bale-publish
    exec ${tests}/bin/registry-acceptance.test -test.v -test.count=1 -test.timeout=5m "$@"
  '';
}
