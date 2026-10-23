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
  artifactType ? null,
  config ? null,
  attachments ? [ ],
}:
let
  imageTag = "nix";
  normalizeLayers = map (layer: layer // { title = layer.title or (baseNameOf layer.file); });
  payloads = normalizeLayers layers;
  attachmentPayloads = map (
    attachment: attachment // { layers = normalizeLayers attachment.layers; }
  ) attachments;
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
      inherit artifactType config;
      layers = payloads;
      attachments = attachmentPayloads;
    }
  );
in
assert config != null || artifactType != null;
assert validLayers payloads;
assert lib.all (attachment: validLayers attachment.layers) attachmentPayloads;
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
    ${lib.optionalString (config != null) "cp ${lib.escapeShellArg config.file} config.json"}
    mkdir payloads
    cd payloads
    ${lib.concatMapStringsSep "\n" (layer: ''
      cp ${lib.escapeShellArg layer.file} ${lib.escapeShellArg layer.title}
    '') payloads}
    oras push --no-tty --oci-layout "$out:${imageTag}" \
      --image-spec ${if config != null && artifactType == null then "v1.0" else "v1.1"} \
      --annotation org.opencontainers.image.created=1970-01-01T00:00:01Z \
      ${
        lib.optionalString (artifactType != null) "--artifact-type ${lib.escapeShellArg artifactType}"
      } \
      ${
        lib.optionalString (
          config != null
        ) "--config ${lib.escapeShellArg "../config.json:${config.mediaType}"}"
      } \
      ${lib.escapeShellArgs (map (layer: "${layer.title}:${layer.mediaType}") payloads)}
    ${lib.concatImapStringsSep "\n" (index: attachment: ''
      mkdir ../attachment-${toString index}
      cd ../attachment-${toString index}
      ${lib.concatMapStringsSep "\n" (layer: ''
        cp ${lib.escapeShellArg layer.file} ${lib.escapeShellArg layer.title}
      '') attachment.layers}
      oras attach --no-tty --oci-layout "$out:${imageTag}" \
        --artifact-type ${lib.escapeShellArg attachment.artifactType} \
        --annotation org.opencontainers.image.created=1970-01-01T00:00:01Z \
        ${lib.escapeShellArgs (map (layer: "${layer.title}:${layer.mediaType}") attachment.layers)}
    '') attachmentPayloads}
    python3 ${../tests/packaging/oci_artifact.py} "$out" ${expected}
  ''
