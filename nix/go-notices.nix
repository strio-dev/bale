# Preserve the pinned Go toolchain and vendored dependency notices in packages.
{
  go,
  vendorSource,
  destination,
}:
''
  mkdir -p "${destination}/go"
  for file in LICENSE PATENTS; do
    tar -xOf ${go.src} "go/$file" > "${destination}/go/$file"
  done
  if [ -f ${vendorSource}/modules.txt ]; then
    install -Dm644 ${vendorSource}/modules.txt "${destination}/go-modules/modules.txt"
  fi
  while IFS= read -r -d "" file; do
    relative="''${file#${vendorSource}/}"
    install -Dm644 "$file" "${destination}/go-modules/$relative"
  done < <(find ${vendorSource} -type f \( \
    -iname 'license*' -o -iname 'licence*' -o -iname 'copying*' \
    -o -iname 'notice*' -o -iname 'copyright*' -o -iname 'patents*' \
    -o -iname 'authors*' -o -iname 'unlicense*' \
    -o -ipath '*/licenses/*' \) ! -iname '*.go' ! -iname '*.proto' -print0)
  cat > "${destination}/NOTICE" <<'NOTICE'
  This distribution includes Go and third-party software. The go directory
  contains the pinned Go toolchain's LICENSE and PATENTS. The go-modules
  directory preserves vendored dependency license and notice files at their
  module-relative paths; modules.txt identifies the pinned vendored modules.
  This inventory may include dependencies used only during builds or tests.
  Consult the individual files for their applicable terms and attributions.
  NOTICE
''
