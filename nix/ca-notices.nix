{
  lib,
  runCommand,
  cacert,
}:
# Follow the pinned CA package's source and attribution when nixpkgs updates.
runCommand "bale-mozilla-ca-notices-${cacert.version}" { } ''
  install -Dm444 ${cacert.src} "$out/certdata.txt"
  cat > "$out/NOTICE" <<'NOTICE'
  The CA bundle is built from Mozilla NSS certdata.txt by the Nixpkgs
  nss-cacert ${cacert.version} package. The original source and its notices
  are included as certdata.txt. That source identifies the Mozilla Public
  License, version 2.0, available at https://mozilla.org/MPL/2.0/.

  Pinned source URLs:
  ${lib.concatStringsSep "\n" cacert.src.urls}

  Root store documentation:
  ${cacert.meta.homepage}
  NOTICE
  chmod 444 "$out/NOTICE"
''
