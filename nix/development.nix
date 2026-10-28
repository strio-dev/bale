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
  stdenv,
}:
let
  modules = [
    "."
    "plugins/helm"
    "plugins/helm-relocate"
  ];
  goPaths = [
    "cmd"
    "internal"
    "plugin"
    "store"
    "tests"
  ]
  ++ builtins.tail modules;
  formatSource = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions (
      map (path: lib.fileset.fileFilter (file: file.hasExt "go") (../. + "/${path}")) goPaths
      ++ map (name: ../. + "/${name}") (
        builtins.filter (name: lib.hasSuffix ".go" name) (builtins.attrNames (builtins.readDir ../.))
      )
      ++ [
        ../flake.nix
        (lib.fileset.fileFilter (file: file.hasExt "nix") ./.)
        ../Justfile
      ]
    );
  };
  # The same formatter inputs are used by checkout commands and flake checks.
  format = ''
    gofmt_args=(-w)
    nixfmt_args=()
    just_args=()
    if [[ "''${1:-}" == --check ]]; then
      shift
      gofmt_args=(-l)
      nixfmt_args=(--check)
      just_args=(--check)
    fi
    unformatted="$({
      find . -maxdepth 1 -type f -name '*.go' -print0
      find ${lib.escapeShellArgs goPaths} -type f -name '*.go' -print0
    } | xargs -0 -r gofmt "''${gofmt_args[@]}" "$@")"
    if [[ -n "$unformatted" ]]; then
      printf 'Unformatted Go files:\n%s\n' "$unformatted" >&2
      exit 1
    fi
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
  inherit formatSource format;
  commands = {
    test = command "test" [ ] (eachModule ''go test ./... "$@"'');
    test-race = command "test-race" [ stdenv.cc ] ''
      export CGO_ENABLED=1
      go test -race ./... "$@"
    '';
    vet = command "vet" [ ] (eachModule ''go vet ./... "$@"'');
    lint = command "lint" [ golangci-lint ] (eachModule ''golangci-lint run "$@"'');
    vuln = command "vuln" [ govulncheck ] (eachModule ''govulncheck "$@" ./...'');
    codeql-build = command "codeql-build" [ ] ''
      # Preserve CodeQL's tracing wrapper before Nix changes PATH.
      go_binary="''${1:-go}"
      if (( $# )); then shift; fi
      ${eachModule ''"$go_binary" build ./... "$@"''}
    '';
    fmt = command "fmt" [ findutils nixfmt just ] format;
  };
}
