{
  runCommand,
  python3,
  curl,
  gnutar,
  gzip,
  gawk,
  shellcheck,
}:
runCommand "bale-installer-check"
  {
    nativeBuildInputs = [
      python3
      curl
      gnutar
      gzip
      gawk
      shellcheck
    ];
  }
  ''
    sh -n ${../docs/install.sh}
    shellcheck ${../docs/install.sh}
    python ${./tests/installer.py} ${../docs/install.sh}
    touch "$out"
  ''
