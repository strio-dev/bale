{
  lib,
  stdenv,
  writeShellApplication,
  coreutils,
  oras,
  regctl,
  publish,
  container-index,
  helm-consumer-archive,
  version,
}:
let
  arch = stdenv.hostPlatform.go.GOARCH;
  imageTag = lib.replaceStrings [ "+" ] [ "_" ] version;
  pluginTag = lib.replaceStrings [ "+" ] [ "_" ] helm-consumer-archive.pluginVersion;
  common = ''
    plain_http=false
    resolve_options=()
    if [[ "''${1:-}" == --plain-http ]]; then
      plain_http=true
      resolve_options=(--plain-http)
      shift
    fi
    validate_repository() {
      local repository="$1" component
      if [[ ! "$repository" =~ ^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+$ ]]; then
        echo 'Expected an explicit registry/repository without a tag or digest.' >&2
        exit 2
      fi
      local path="''${repository#*/}"
      while [[ -n "$path" ]]; do
        component="''${path%%/*}"
        if [[ ! "$component" =~ ^[a-z0-9]+(([._]|__|-+)[a-z0-9]+)*$ ]]; then
          echo 'Invalid repository path.' >&2
          exit 2
        fi
        if [[ "$path" == "$component" ]]; then break; fi
        path="''${path#*/}"
        if [[ -z "$path" ]]; then
          echo 'Invalid repository path.' >&2
          exit 2
        fi
      done
    }
    validate_digest() {
      if [[ ! "$1" =~ ^sha256:[0-9a-f]{64}$ ]]; then
        echo 'Expected a SHA-256 artifact digest.' >&2
        exit 2
      fi
    }
    resolve_digest() {
      local digest
      digest=$(oras resolve "''${resolve_options[@]}" "$1")
      validate_digest "$digest"
      printf '%s\n' "$digest"
    }
  '';
in
assert lib.elem arch [
  "amd64"
  "arm64"
];
{
  integration-stage = writeShellApplication {
    name = "bale-integration-stage";
    runtimeInputs = [
      coreutils
      oras
    ];
    text = common + ''
      if (( $# != 3 )); then
        echo 'Usage: bale-integration-stage [--plain-http] REPOSITORY RUN_KEY OUTPUT_DIR' >&2
        exit 2
      fi
      repository="$1"
      run_key="$2"
      output="$3"
      validate_repository "$repository"
      if [[ ! "$run_key" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,80}$ ]]; then
        echo 'Invalid release run key.' >&2
        exit 2
      fi
      publish_options=()
      if [[ "$plain_http" == true ]]; then
        publish_options=(--to-plain-http)
      fi
      umask 077
      mkdir -- "$output"
      complete=false
      trap 'if [[ "$complete" != true ]]; then rm -rf -- "$output"; fi' EXIT
      stage_artifact() {
        local artifact="$1" target="$2" filename="$3"
        ${publish}/bin/bale-publish "$artifact" "$target" "''${publish_options[@]}" >&2
        resolve_digest "$target" > "$output/$filename"
      }
      stage_artifact bale-flux "$repository-flux:build-$run_key-${arch}" flux-${arch}.digest
      stage_artifact helm-consumer "$repository/plugins/bale:build-$run_key-${arch}" helm-consumer-${arch}.digest
      ${lib.optionalString (arch == "amd64") ''
        stage_artifact helm-discovery "$repository/plugins/helm-discovery:build-$run_key" helm-discovery.digest
      ''}
      complete=true
    '';
  };

  integration-publish = writeShellApplication {
    name = "bale-integration-publish";
    runtimeInputs = [
      coreutils
      oras
      regctl
    ];
    text = common + ''
      if (( $# != 2 )); then
        echo 'Usage: bale-integration-publish [--plain-http] REPOSITORY DIGEST_DIR' >&2
        exit 2
      fi
      repository="$1"
      input="$2"
      image_tag=${lib.escapeShellArg imageTag}
      validate_repository "$repository"
      read_digest() {
        local digest
        digest=$(cat "$input/$1.digest")
        validate_digest "$digest"
        printf '%s\n' "$digest"
      }
      flux_amd64=$(read_digest flux-amd64)
      flux_arm64=$(read_digest flux-arm64)
      consumer_amd64=$(read_digest helm-consumer-amd64)
      consumer_arm64=$(read_digest helm-consumer-arm64)
      discovery=$(read_digest helm-discovery)

      verify_staged() {
        local staged_repository="$1" digest="$2"
        if [[ "$(resolve_digest "$staged_repository@$digest")" != "$digest" ]]; then
          echo 'Staged artifact does not match its supplied digest.' >&2
          exit 1
        fi
      }
      verify_staged "$repository-flux" "$flux_amd64"
      verify_staged "$repository-flux" "$flux_arm64"
      verify_staged "$repository/plugins/bale" "$consumer_amd64"
      verify_staged "$repository/plugins/bale" "$consumer_arm64"
      verify_staged "$repository/plugins/helm-discovery" "$discovery"

      copy_options=()
      chart_options=()
      index_options=()
      if [[ "$plain_http" == true ]]; then
        copy_options=(--from-plain-http --to-plain-http)
        chart_options=(--plain-http)
        index_options=(--host "reg=''${repository%%/*},tls=disabled")
      fi

      flux_digest=$(${container-index}/bin/bale-container-index "$repository-flux" "$image_tag" "$flux_amd64" "$flux_arm64" "''${index_options[@]}")
      validate_digest "$flux_digest"
      if [[ "$(resolve_digest "$repository-flux:$image_tag")" != "$flux_digest" ]]; then
        echo 'Published Flux index does not match its confirmed digest.' >&2
        exit 1
      fi
      promote_artifact() {
        local target_repository="$1" digest="$2" tag="$3"
        oras cp --recursive "''${copy_options[@]}" "$target_repository@$digest" "$target_repository:$tag" >&2
        if [[ "$(resolve_digest "$target_repository:$tag")" != "$digest" ]]; then
          echo 'Published artifact does not match its staged digest.' >&2
          exit 1
        fi
      }
      promote_artifact "$repository/plugins/helm-discovery" "$discovery" "$image_tag"
      promote_artifact "$repository/plugins/bale" "$consumer_amd64" ${lib.escapeShellArg "${pluginTag}-linux-amd64"}
      promote_artifact "$repository/plugins/bale" "$consumer_arm64" ${lib.escapeShellArg "${pluginTag}-linux-arm64"}
      ${publish}/bin/bale-publish bale-flux-chart "oci://$repository/charts" "''${chart_options[@]}" >&2
      printf '%s\n' "$flux_digest"
    '';
  };
}
