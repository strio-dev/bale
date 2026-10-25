{
  writeShellApplication,
  coreutils,
  skopeo,
  regctl,
  container,
}:
{
  container-push = writeShellApplication {
    name = "bale-container-push";
    runtimeInputs = [ skopeo ];
    text = ''
      if (( $# < 1 )); then
        echo 'Usage: bale-container-push DESTINATION [Skopeo copy options]' >&2
        exit 2
      fi
      destination="$1"
      shift
      # Signature policy applies to this trusted Nix-built layout. Registry
      # certificate verification and authentication remain enabled.
      exec skopeo --insecure-policy copy --format oci "$@" \
        oci:${container}:${container.imageTag} "docker://$destination"
    '';
  };
  container-index = writeShellApplication {
    name = "bale-container-index";
    runtimeInputs = [ regctl ];
    text = ''
      if (( $# < 4 )); then
        echo 'Usage: bale-container-index REPOSITORY TAG AMD64_DIGEST ARM64_DIGEST [regctl options]' >&2
        exit 2
      fi
      [[ "$3" =~ ^sha256:[0-9a-f]{64}$ ]]
      [[ "$4" =~ ^sha256:[0-9a-f]{64}$ ]]
      repository="$1"
      tag="$2"
      amd64="$3"
      arm64="$4"
      shift 4
      exec regctl "$@" index create "$repository:$tag" \
        --ref "$repository@$amd64" --platform linux/amd64 \
        --ref "$repository@$arm64" --platform linux/arm64 \
        --format '{{.Manifest.GetDescriptor.Digest}}'
    '';
  };
  container-latest = writeShellApplication {
    name = "bale-container-latest";
    runtimeInputs = [ regctl ];
    text = ''
      if (( $# != 2 )); then
        echo 'Usage: bale-container-latest REPOSITORY VERSION_OR_DIGEST' >&2
        exit 2
      fi
      reference="$2"
      if [[ "$reference" != sha256:* ]]; then
        reference=$(regctl image digest "$1:$reference")
      fi
      [[ "$reference" =~ ^sha256:[0-9a-f]{64}$ ]]
      exec regctl image copy "$1@$reference" "$1:latest"
    '';
  };
  release-checksums = writeShellApplication {
    name = "bale-release-checksums";
    runtimeInputs = [ coreutils ];
    text = ''
      if (( $# != 1 )); then
        echo 'Usage: bale-release-checksums DIRECTORY' >&2
        exit 2
      fi
      cd "$1"
      test -f bale-linux-amd64.tar.gz
      test -f bale-linux-arm64.tar.gz
      sha256sum ./*.tar.gz > SHA256SUMS
    '';
  };
}
