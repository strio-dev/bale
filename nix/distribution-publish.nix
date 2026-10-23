{
  writeShellApplication,
  oras,
  kubernetes-helm,
  container,
  helm-plugin-image,
  helm-consumer-image,
  flux-service-image,
  flux-service-chart,
}:
writeShellApplication {
  name = "bale-publish";
  runtimeInputs = [
    oras
    kubernetes-helm
  ];
  text = ''
    if (( $# < 2 )); then
      echo 'Usage: bale-publish ARTIFACT DESTINATION [publisher options]' >&2
      echo 'Artifacts: bale, helm-discovery, helm-consumer, bale-flux, bale-flux-chart' >&2
      exit 2
    fi
    artifact="$1"
    destination="$2"
    shift 2
    case "$artifact" in
      bale) source=${container}:${container.imageTag} ;;
      helm-discovery) source=${helm-plugin-image}:${helm-plugin-image.imageTag} ;;
      helm-consumer)
        if [[ "''${destination##*/}" != bale:* ]]; then
          echo 'The Helm plugin destination must end with /bale:TAG.' >&2
          exit 2
        fi
        source=${helm-consumer-image}:${helm-consumer-image.imageTag}
        ;;
      bale-flux) source=${flux-service-image}:${flux-service-image.imageTag} ;;
      bale-flux-chart)
        exec helm push ${flux-service-chart}/bale-flux-${flux-service-chart.chartVersion}.tgz "$destination" "$@"
        ;;
      *) echo "Unknown distribution artifact: $artifact" >&2; exit 2 ;;
    esac
    exec oras cp --recursive --from-oci-layout "$@" "$source" "$destination"
  '';
}
