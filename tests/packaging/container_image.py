"""Check the distributable CLI image against its packaged inputs, without extracting it."""

import hashlib
import io
import json
from pathlib import Path, PurePosixPath
import struct
import sys
import tarfile


layout, package, certificates = map(Path, sys.argv[1:4])
version, architecture = sys.argv[4:6]
ca_notices = Path(sys.argv[6])
assert json.loads((layout / "oci-layout").read_text())["imageLayoutVersion"] == "1.0.0"


def blob(descriptor):
    algorithm, digest = descriptor["digest"].split(":", 1)
    assert algorithm == "sha256"
    raw = (layout / "blobs" / algorithm / digest).read_bytes()
    assert len(raw) == descriptor["size"]
    assert hashlib.sha256(raw).hexdigest() == digest
    return raw


index = json.loads((layout / "index.json").read_bytes())
assert index["schemaVersion"] == 2
assert len(index["manifests"]) == 1
descriptor = index["manifests"][0]
assert descriptor["annotations"]["org.opencontainers.image.ref.name"] == "nix"
manifest = json.loads(blob(descriptor))
assert manifest["mediaType"] == "application/vnd.oci.image.manifest.v1+json"
assert manifest["schemaVersion"] == 2
config = json.loads(blob(manifest["config"]))
assert config["os"] == "linux" and config["architecture"] == architecture
settings = config["config"]
assert settings["Entrypoint"] == ["/bin/bale"]
assert not settings.get("Cmd")
assert settings["User"] == "65532:65532"
assert settings["WorkingDir"] == "/work"
assert set(settings["Env"]) == {
    "HOME=/tmp",
    "XDG_CACHE_HOME=/cache",
    "TMPDIR=/tmp",
    "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
}
assert settings["Labels"]["org.opencontainers.image.version"] == version
assert settings["Labels"]["org.opencontainers.image.source"] == "https://github.com/strio-dev/bale"
assert settings["Labels"]["org.opencontainers.image.licenses"] == "MIT"
assert not settings.get("ExposedPorts") and not settings.get("Volumes")

files = {}
entries = {}
diff_ids = []
for layer in manifest["layers"]:
    with tarfile.open(fileobj=io.BytesIO(blob(layer)), mode="r:*") as archive:
        # Diff IDs hash the uncompressed tar stream, including its padding.
        archive.fileobj.seek(0)
        diff_ids.append("sha256:" + hashlib.sha256(archive.fileobj.read()).hexdigest())
        archive.fileobj.seek(0)
        for entry in archive:
            path = PurePosixPath(entry.name)
            assert not path.is_absolute() and ".." not in path.parts, entry.name
            name = str(path)
            assert not name.startswith("nix/"), "image includes a Nix store closure"
            assert not any(part.startswith(".wh.") for part in path.parts), "unexpected layer whiteout"
            assert entry.isdir() or entry.isfile(), "image contains a link or special device"
            assert entry.uid == entry.gid == 0, "image files must have stable root ownership"
            assert not entry.mode & 0o6000, "image has a setuid/setgid entry"
            entries[name] = entry
            if entry.isfile():
                files[name] = archive.extractfile(entry).read()
assert config["rootfs"]["type"] == "layers"
assert config["rootfs"]["diff_ids"] == diff_ids

binary = files["bin/bale"]
assert binary == (package / "bin/bale").read_bytes()
assert binary[:6] == b"\x7fELF\x02\x01", "expected a 64-bit little-endian ELF"
assert struct.unpack_from("<H", binary, 18)[0] == {"amd64": 62, "arm64": 183}[architecture]
# Program headers independently prove no runtime dynamic linker is needed.
offset = struct.unpack_from("<Q", binary, 32)[0]
size, count = struct.unpack_from("<HH", binary, 54)
assert all(struct.unpack_from("<I", binary, offset + i * size)[0] != 3 for i in range(count))
assert entries["bin/bale"].mode == 0o555
assert files["etc/ssl/certs/ca-certificates.crt"] == certificates.read_bytes()
assert b"bale:x:65532:65532:" in files["etc/passwd"]
assert b"bale:x:65532:" in files["etc/group"]
for directory in ("work", "tmp", "cache"):
    assert entries[directory].isdir() and entries[directory].mode == 0o1777

licenses = package / "share/licenses/bale"
expected_licenses = {
    "usr/share/licenses/bale/" + str(path.relative_to(licenses)): path.read_bytes()
    for path in licenses.rglob("*")
    if path.is_file()
}
assert "usr/share/licenses/bale/LICENSE" in expected_licenses
assert "usr/share/licenses/bale/third-party/NOTICE" in expected_licenses
assert "usr/share/licenses/bale/third-party/go/LICENSE" in expected_licenses
assert "usr/share/licenses/bale/third-party/go/PATENTS" in expected_licenses
assert "usr/share/licenses/bale/third-party/go-modules/modules.txt" in expected_licenses
expected_licenses.update({
    "usr/share/licenses/mozilla-ca/" + name: (ca_notices / name).read_bytes()
    for name in ("NOTICE", "certdata.txt")
})
assert b"Source Code Form" in expected_licenses["usr/share/licenses/mozilla-ca/certdata.txt"]
assert {path: data for path, data in files.items() if path.startswith("usr/share/licenses/")} == expected_licenses
assert set(files) == {
    "bin/bale", "etc/passwd", "etc/group", "etc/ssl/certs/ca-certificates.crt",
} | expected_licenses.keys(), "image contains unexpected runtime files"
print("Bale OCI image verified: static CLI, CA roots, complete notices, non-root and arbitrary-UID directories")
