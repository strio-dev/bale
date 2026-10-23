{
  callPackage,
  runCommand,
  python3,
  gnutar,
  gzip,
  helm-plugin,
}:
let
  module = "${helm-plugin}/lib/bale/plugins/helm.wasm";
  licenses =
    runCommand "bale-helm-discovery-licenses.tar.gz"
      {
        nativeBuildInputs = [
          gnutar
          gzip
        ];
      }
      ''
        tar --sort=name --mtime=@1 --owner=0 --group=0 --numeric-owner \
          -C ${helm-plugin}/share/licenses/bale-helm-plugin -cf - . | gzip -n > "$out"
      '';
  config = runCommand "bale-helm-discovery-config.json" { nativeBuildInputs = [ python3 ]; } ''
    python3 - ${module} "$out" <<'PY'
    import hashlib
    import json
    from pathlib import Path
    import sys

    module = Path(sys.argv[1]).read_bytes()
    assert 8 <= len(module) <= 128 << 20, "plugin must be at most 128 MiB"
    assert module[:8] == b"\x00asm\x01\x00\x00\x00", "plugin must be a core Wasm module"
    config = {
        "architecture": "wasm",
        "os": "wasip1",
        "layerDigests": ["sha256:" + hashlib.sha256(module).hexdigest()],
    }
    Path(sys.argv[2]).write_text(json.dumps(config, separators=(",", ":")))
    PY
  '';
in
(callPackage ./oci-artifact.nix { }) {
  name = "bale-helm-discovery-oci-${helm-plugin.version}";
  config = {
    file = "${config}";
    mediaType = "application/vnd.wasm.config.v0+json";
  };
  layers = [
    {
      file = module;
      title = "helm.wasm";
      mediaType = "application/wasm";
    }
  ];
  attachments = [
    {
      artifactType = "application/gzip";
      layers = [
        {
          file = "${licenses}";
          title = "helm-discovery-licenses.tar.gz";
          mediaType = "application/gzip";
        }
      ];
    }
  ];
}
