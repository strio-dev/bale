{
  lib,
  dockerTools,
  runCommand,
  skopeo,
  python3,
  binutils,
  cacert,
  flux-service,
  version ? "dev",
}:
let
  caNotices = import ./ca-notices.nix { inherit lib runCommand cacert; };
  archive = dockerTools.buildLayeredImage {
    name = "bale-flux";
    tag = version;
    compressor = "none";
    contents = [ ];
    includeStorePaths = false;
    created = "1970-01-01T00:00:01Z";
    mtime = "1970-01-01T00:00:01Z";
    extraCommands = ''
      install -Dm555 ${flux-service}/bin/bale-flux bin/bale-flux
      install -Dm444 ${cacert}/etc/ssl/certs/ca-bundle.crt etc/ssl/certs/ca-certificates.crt
      install -Dm444 ${../LICENSE} usr/share/licenses/bale/LICENSE
      cp -r ${flux-service}/share/licenses/bale-flux-service/third-party usr/share/licenses/bale/third-party
      cp -r ${caNotices} usr/share/licenses/mozilla-ca
      mkdir -p home/bale tmp
      chmod 755 home home/bale
      chmod 1777 tmp
      printf '%s\n' 'bale:x:65532:65532:Bale:/home/bale:/sbin/nologin' > etc/passwd
      printf '%s\n' 'bale:x:65532:' > etc/group
      chmod 444 etc/passwd etc/group
    '';
    config = {
      Entrypoint = [ "/bin/bale-flux" ];
      User = "65532:65532";
      WorkingDir = "/home/bale";
      Env = [
        "HOME=/home/bale"
        "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt"
      ];
      ExposedPorts."8080/tcp" = { };
      Labels = {
        "org.opencontainers.image.title" = "Bale Flux adapter";
        "org.opencontainers.image.source" = "https://github.com/strio-dev/bale";
        "org.opencontainers.image.licenses" = "MIT";
        "org.opencontainers.image.version" = version;
      };
    };
  };
in
runCommand "bale-flux-image-${version}"
  {
    nativeBuildInputs = [
      skopeo
      python3
      binutils
    ];
    passthru.imageTag = version;
    meta = {
      description = "OCI image layout for the Bale Flux adapter";
      license = lib.licenses.mit;
      platforms = lib.platforms.linux;
    };
  }
  ''
    readelf -l ${flux-service}/bin/bale-flux > program-headers
    if grep -q INTERP program-headers; then
      echo 'bale-flux must be statically linked before packaging' >&2
      exit 1
    fi
    skopeo --insecure-policy copy --format oci \
      docker-archive:${archive} oci:$out:${version}
    python3 ${../tests/packaging/flux_image.py} "$out" \
      ${flux-service}/bin/bale-flux ${cacert}/etc/ssl/certs/ca-bundle.crt ${caNotices}
  ''
