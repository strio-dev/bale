"""Verify Helm's distributable archive and installation preserve the native plugin."""

from pathlib import Path, PurePosixPath
import struct
import sys
import tarfile


archive, source, installed = map(Path, sys.argv[1:4])
version, architecture = sys.argv[4:6]
cli_list, renderer_list = map(Path, sys.argv[6:8])
expected = {}
for path in source.rglob("*"):
    assert not path.is_symlink(), "plugin input contains a filesystem link"
    if path.is_file():
        expected["bale/" + path.relative_to(source).as_posix()] = path.read_bytes()
for required in (
    "plugin.yaml",
    "bale-helm",
    "bale-relocate",
    "postrenderers/bale-postrenderer/plugin.yaml",
    "LICENSE",
    "third-party/NOTICE",
    "third-party/go/LICENSE",
    "third-party/go/PATENTS",
    "third-party/go-modules/modules.txt",
):
    assert "bale/" + required in expected, required

files = {}
entries = {}
with tarfile.open(archive, "r:gz") as tar:
    for entry in tar:
        path = PurePosixPath(entry.name)
        assert not path.is_absolute() and ".." not in path.parts, entry.name
        assert path.parts[0] == "bale", "unexpected archive root"
        assert entry.isdir() or entry.isfile(), "archive contains a link or special file"
        assert entry.name not in entries, "archive contains a duplicate path"
        assert not entry.mode & 0o6000, "archive has a setuid/setgid entry"
        entries[entry.name] = entry
        if entry.isfile():
            files[entry.name] = tar.extractfile(entry).read()
assert files == expected, "archive content differs from the packaged plugin"

for name in ("bale-helm", "bale-relocate"):
    binary = files["bale/" + name]
    assert entries["bale/" + name].mode & 0o111, "native plugin is not executable"
    assert binary[:6] == b"\x7fELF\x02\x01", "expected a 64-bit little-endian ELF"
    assert struct.unpack_from("<H", binary, 18)[0] == {"amd64": 62, "arm64": 183}[architecture]
    offset = struct.unpack_from("<Q", binary, 32)[0]
    size, count = struct.unpack_from("<HH", binary, 54)
    assert all(struct.unpack_from("<I", binary, offset + i * size)[0] != 3 for i in range(count)), "plugin requires a dynamic loader"

actual = {
    "bale/" + path.relative_to(installed).as_posix(): path.read_bytes()
    for path in installed.rglob("*")
    if path.is_file()
}
assert actual == expected, "Helm installation changed or omitted plugin files"
for name in ("bale-helm", "bale-relocate"):
    assert (installed / name).stat().st_mode & 0o111, "installed plugin is not executable"

for listing, name, kind in (
    (cli_list, "bale", "cli/v1"),
    (renderer_list, "bale-postrenderer", "postrenderer/v1"),
):
    rows = [line.split() for line in listing.read_text().splitlines()[1:] if line.strip()]
    assert len(rows) == 1 and rows[0][:4] == [name, version, kind, "v1"], rows
print("Helm archive verified: native architecture, complete plugin and notices, stable bytes, stock Helm installation")
