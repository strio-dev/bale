{
  lib,
  stdenv,
  dockerTools,
  runCommand,
  skopeo,
  python3,
  binutils,
  cacert,
  bale,
}:
let
  # Keep the local OCI tag independent of SemVer (which may contain '+').
  imageTag = "nix";
  caNotices = import ./ca-notices.nix { inherit lib runCommand cacert; };
  archive = dockerTools.buildLayeredImage {
    name = "bale";
    tag = imageTag;
    compressor = "none";
    contents = [ ];
    includeStorePaths = false;
    created = "1970-01-01T00:00:01Z";
    mtime = "1970-01-01T00:00:01Z";
    extraCommands = ''
      install -Dm555 ${bale}/bin/bale bin/bale
      install -Dm444 ${cacert}/etc/ssl/certs/ca-bundle.crt etc/ssl/certs/ca-certificates.crt
      mkdir -p usr/share/licenses
      cp -r ${bale}/share/licenses/bale usr/share/licenses/bale
      cp -r ${caNotices} usr/share/licenses/mozilla-ca
      mkdir -p work tmp cache
      chmod 1777 work tmp cache
      printf '%s\n' 'bale:x:65532:65532:Bale:/tmp:/sbin/nologin' > etc/passwd
      printf '%s\n' 'bale:x:65532:' > etc/group
      chmod 444 etc/passwd etc/group
    '';
    config = {
      Entrypoint = [ "/bin/bale" ];
      User = "65532:65532";
      WorkingDir = "/work";
      Env = [
        "HOME=/tmp"
        "XDG_CACHE_HOME=/cache"
        "TMPDIR=/tmp"
        "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt"
      ];
      Labels = {
        "org.opencontainers.image.title" = "Bale";
        "org.opencontainers.image.description" =
          "Bundle OCI images and artifacts for registry and offline transfer";
        "org.opencontainers.image.source" = "https://github.com/strio-dev/bale";
        "org.opencontainers.image.licenses" = "MIT";
        "org.opencontainers.image.version" = bale.version;
      };
    };
  };
in
runCommand "bale-container-${bale.version}"
  {
    nativeBuildInputs = [
      skopeo
      python3
      binutils
    ];
    passthru = { inherit imageTag; };
    meta = {
      description = "Self-contained OCI image layout for the Bale CLI";
      license = lib.licenses.mit;
      platforms = lib.platforms.linux;
    };
  }
  ''
    readelf -l ${bale}/bin/bale > program-headers
    if grep -q INTERP program-headers; then
      echo 'bale must be statically linked before packaging' >&2
      exit 1
    fi
    skopeo --insecure-policy copy --format oci \
      docker-archive:${archive} oci:$out:${imageTag}
    python3 ${../tests/packaging/container_image.py} "$out" \
      ${bale} ${cacert}/etc/ssl/certs/ca-bundle.crt \
      ${lib.escapeShellArg bale.version} ${stdenv.hostPlatform.go.GOARCH} ${caNotices}
  ''
