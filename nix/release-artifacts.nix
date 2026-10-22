{ pkgs, bale }:
let
  arch =
    {
      x86_64-linux = "amd64";
      aarch64-linux = "arm64";
    }
    .${pkgs.stdenv.hostPlatform.system};
  binary =
    pkgs.runCommand "bale-linux-${arch}.tar.gz"
      {
        nativeBuildInputs = [
          pkgs.gnutar
          pkgs.gzip
        ];
      }
      ''
        mkdir package "$out"
        cp ${bale}/bin/bale package/bale
        cp ${bale}/share/licenses/bale/LICENSE package/LICENSE
        cp -r ${bale}/share/licenses/bale/third-party package/third-party
        tar --sort=name --mtime=@1 --owner=0 --group=0 --numeric-owner \
          -C package -cf - bale LICENSE third-party | gzip -n > "$out/bale-linux-${arch}.tar.gz"
      '';
in
{
  inherit binary;
  check =
    pkgs.runCommand "bale-release-check"
      {
        nativeBuildInputs = [
          pkgs.gnutar
          pkgs.gzip
          pkgs.binutils
        ];
      }
      ''
        mkdir package
        tar -xzf ${binary}/bale-linux-${arch}.tar.gz -C package
        cmp package/bale ${bale}/bin/bale
        cmp package/LICENSE ${../LICENSE}
        diff -r package/third-party ${bale}/share/licenses/bale/third-party
        test -s package/third-party/NOTICE
        test -s package/third-party/go/LICENSE
        test -s package/third-party/go-modules/oras.land/oras-go/v2/LICENSE
        if readelf -l package/bale | grep -q INTERP; then
          echo 'The release binary requires a dynamic loader' >&2
          exit 1
        fi
        env -i PATH=/nonexistent HOME="$TMPDIR" ./package/bale --help > help
        grep -q 'Usage:' help
        test "$(env -i PATH=/nonexistent HOME="$TMPDIR" ./package/bale --version)" = ${pkgs.lib.escapeShellArg "bale ${bale.version}"}
        touch "$out"
      '';
}
