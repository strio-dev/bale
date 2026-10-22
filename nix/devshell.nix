{
  pkgs,
  bale,
  zensical,
  go_1_27,
}:
let
  sourceBale = pkgs.writeShellScriptBin "bale" ''
    root="$(git rev-parse --show-toplevel 2>/dev/null)" || { echo 'bale: run inside the Bale checkout' >&2; exit 1; }
    test -f "$root/cmd/bale/main.go" || { echo 'bale: not a Bale checkout' >&2; exit 1; }
    cd "$root" || exit 1
    exec ${go_1_27}/bin/go run ./cmd/bale "$@"
  '';
in
pkgs.mkShell {
  inputsFrom = [ bale ];
  packages = with pkgs; [
    sourceBale
    go_1_27
    git
    just
    gopls
    gotools
    golangci-lint
    govulncheck
    delve
    nixfmt
    actionlint
    shellcheck
    zensical
  ];
  CGO_ENABLED = "0";
}
