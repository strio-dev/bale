{
  lib,
  strio-theme,
  system,
}:
strio-theme.lib.mkZensicalSite {
  inherit system;
  name = "bale-docs";
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../docs
      ../zensical.toml
    ];
  };
}
