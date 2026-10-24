{
  lib,
  runCommand,
  writeText,
  oras,
  python3,
}:
{
  name,
  layers,
  artifactType,
}:
let
  imageTag = "nix";
  normalizeLayers = map (layer: layer // { title = layer.title or (baseNameOf layer.file); });
  payloads = normalizeLayers layers;
  validLayers =
    layers:
    let
      titles = map (layer: layer.title) layers;
    in
    layers != [ ]
    && lib.length (lib.unique titles) == lib.length titles
    && lib.all (title: title != "" && title == baseNameOf title && !lib.hasInfix ":" title) titles;
  expected = writeText "${name}-inputs.json" (
    builtins.toJSON {
      inherit artifactType;
      layers = payloads;
    }
  );
in
assert validLayers payloads;
runCommand name
  {
    nativeBuildInputs = [
      oras
      python3
    ];
    passthru = { inherit imageTag; };
  }
  ''
    export HOME="$TMPDIR"
    mkdir payloads
    cd payloads
    ${lib.concatMapStringsSep "\n" (layer: ''
      cp ${lib.escapeShellArg layer.file} ${lib.escapeShellArg layer.title}
    '') payloads}
    oras push --no-tty --oci-layout "$out:${imageTag}" \
      --image-spec v1.1 \
      --annotation org.opencontainers.image.created=1970-01-01T00:00:01Z \
      --artifact-type ${lib.escapeShellArg artifactType} \
      ${lib.escapeShellArgs (map (layer: "${layer.title}:${layer.mediaType}") payloads)}
    python3 ${../tests/packaging/oci_artifact.py} "$out" ${expected}
  ''
