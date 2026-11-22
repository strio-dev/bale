{ go_1_27, fetchurl }:
# Keep the pinned Nix environment while taking Go's GO-2026-6859 security fix.
go_1_27.overrideAttrs {
  version = "1.27.4";
  src = fetchurl {
    url = "https://go.dev/dl/go1.27.4.src.tar.gz";
    hash = "sha256-c8+4VxrsDAEXdofbhxY9Rqog6XeWcncokaoXZU2aYJk=";
  };
}
