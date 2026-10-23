{
  fetchFromGitHub,
  jq,
  mkShellNoCC,
  runCommand,
  semgrep,
  writeShellApplication,
  git,
  coreutils,
}:
let
  source = fetchFromGitHub {
    owner = "semgrep";
    repo = "semgrep-rules";
    rev = "9bbf021d0acffa3095fcffd17b2bc7787eb4796d";
    hash = "sha256-3PbjZ07ABF6aBCGmARihUqkL5EsvF4dkkQFdCjpyNfY=";
  };
  rules = runCommand "bale-semgrep-rules" { } ''
    mkdir -p "$out/python/lang" "$out/yaml/github-actions"
    cd ${source}
    find go -type d -name security -prune \
      -exec cp -r --no-preserve=mode --parents -t "$out" {} +
    cp -r python/lang/security "$out/python/lang"
    cp -r yaml/github-actions/security "$out/yaml/github-actions"
    cp LICENSE "$out/LICENSE"
    # Explicit root anchoring preserves this rule under Semgrepignore v2.
    rule="$out/yaml/github-actions/security/audit/third-party-action-not-pinned-to-commit-sha.yml"
    chmod u+w "$rule"
    substituteInPlace "$rule" --replace-fail '".github/workflows"' '"/.github/workflows"'
  '';
in
{
  scan = writeShellApplication {
    name = "bale-scan";
    runtimeInputs = [
      semgrep
      jq
      git
      coreutils
    ];
    text = ''
      root="$(git rev-parse --show-toplevel 2>/dev/null)" || { echo 'Run inside the Bale checkout.' >&2; exit 1; }
      test -f "$root/cmd/bale/main.go"
      cd "$root"
      mkdir -p .cache
      ln -sfnT ${rules} .cache/semgrep-rules
      export SEMGREP_RULES=.cache/semgrep-rules
      export SEMGREP_SEND_METRICS=off
      export SEMGREP_ENABLE_VERSION_CHECK=0
      error_mode=--error
      case "''${1:-}" in
        --error|--no-error) error_mode="$1"; shift ;;
      esac
      status=0
      semgrep scan "$error_mode" --strict --jobs 2 \
        --exclude 'charts/bale-flux/templates/**' \
        --sarif-output .cache/semgrep.raw.sarif \
        "$@" . nix/tests/installer.py tests/packaging/*.py || status=$?
      # GitHub does not consume SARIF suppressions; honor inline annotations.
      jq ' .runs[].results |= map(select(
        any(.suppressions[]?; .kind == "inSource" and (.status // "accepted") == "accepted") | not
      ))' .cache/semgrep.raw.sarif > .cache/semgrep.sarif
      exit "$status"
    '';
  };
  shell = mkShellNoCC {
    packages = [
      semgrep
      jq
      coreutils
    ];
    # Relative config paths keep rule namespaces stable across Nix store hashes.
    SEMGREP_RULES = ".cache/semgrep-rules";
    SEMGREP_SEND_METRICS = "off";
    SEMGREP_ENABLE_VERSION_CHECK = "0";
    shellHook = ''
      mkdir -p .cache && ln -sfnT ${rules} .cache/semgrep-rules || exit 1
    '';
  };
}
