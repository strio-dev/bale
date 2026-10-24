{
  lib,
  writeShellApplication,
  go_1_27,
  git,
  findutils,
  golangci-lint,
  govulncheck,
  nixfmt,
  just,
}:
let
  modules = [
    "."
    "plugins/helm"
    "plugins/helm-relocate"
  ];
  goPaths = [
    "."
    "cmd"
    "internal"
    "plugin"
    "store"
    "tests"
  ]
  ++ builtins.tail modules;
  # The same formatter inputs are used by checkout commands and flake checks.
  format = ''
    find ${lib.escapeShellArgs goPaths} -maxdepth 0 -type d -print0 | while IFS= read -r -d "" directory; do
      if [[ "$directory" == . ]]; then
        find . -maxdepth 1 -name '*.go' -print0
      else
        find "$directory" -name '*.go' -print0
      fi
    done | xargs -0 -r gofmt "$@"
    find flake.nix nix -type f -name '*.nix' -print0 | xargs -0 -r nixfmt "''${nixfmt_args[@]}"
    just --fmt "''${just_args[@]}"
  '';
  command =
    name: runtimeInputs: text:
    writeShellApplication {
      name = "bale-dev-${name}";
      runtimeInputs = [
        git
        go_1_27
      ]
      ++ runtimeInputs;
      text = ''
        root="$(git rev-parse --show-toplevel 2>/dev/null)" || { echo 'Run inside the Bale checkout.' >&2; exit 1; }
        test -f "$root/cmd/bale/main.go"
        cd "$root"
        export CGO_ENABLED=0
        export GOTOOLCHAIN=local
        ${text}
      '';
    };
  eachModule = args: ''
    for module in ${lib.escapeShellArgs modules}; do
      (cd "$module" && ${args})
    done
  '';
in
{
  inherit modules goPaths format;
  commands = {
    test = command "test" [ ] (eachModule ''go test ./... "$@"'');
    vet = command "vet" [ ] (eachModule ''go vet ./... "$@"'');
    lint = command "lint" [ golangci-lint ] (eachModule ''golangci-lint run "$@"'');
    vuln = command "vuln" [ govulncheck ] (eachModule ''govulncheck "$@" ./...'');
    codeql-build = command "codeql-build" [ ] ''
      # Preserve CodeQL's tracing wrapper before Nix changes PATH.
      go_binary="''${1:-go}"
      if (( $# )); then shift; fi
      ${eachModule ''"$go_binary" build ./... "$@"''}
    '';
    fmt = command "fmt" [ findutils nixfmt just ] ''
      set -- -w "$@"
      nixfmt_args=()
      just_args=()
      ${format}
    '';
  };
}
