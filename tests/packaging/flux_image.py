"""Verify the shipped image is self-contained and preserves its build inputs."""

import hashlib
import io
import json
from pathlib import Path
import sys
import tarfile


layout, executable, certificates, ca_notices = map(Path, sys.argv[1:])
assert json.loads((layout / "oci-layout").read_text())["imageLayoutVersion"] == "1.0.0"


def blob(descriptor):
    algorithm, digest = descriptor["digest"].split(":", 1)
    raw = (layout / "blobs" / algorithm / digest).read_bytes()
    assert len(raw) == descriptor["size"]
    assert hashlib.new(algorithm, raw).hexdigest() == digest
    return raw


index = json.loads((layout / "index.json").read_bytes())
assert len(index["manifests"]) == 1
manifest = json.loads(blob(index["manifests"][0]))
assert manifest["mediaType"] == "application/vnd.oci.image.manifest.v1+json"
config = json.loads(blob(manifest["config"]))
assert config["os"] == "linux" and config["architecture"] in ("amd64", "arm64")
assert config["config"]["Entrypoint"] == ["/bin/bale-flux"]
assert config["config"]["User"] == "65532:65532"
assert "HOME=/home/bale" in config["config"]["Env"]
assert "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt" in config["config"]["Env"]
files = {}
for layer in manifest["layers"]:
    with tarfile.open(fileobj=io.BytesIO(blob(layer)), mode="r:*") as archive:
        for entry in archive:
            path = entry.name.removeprefix("./").lstrip("/")
            assert not path.startswith("nix/store/"), "image includes a Nix store closure"
            assert not entry.issym() and not entry.islnk(), "image contains a filesystem link"
            if entry.isfile():
                files[path] = archive.extractfile(entry).read()
assert files["bin/bale-flux"] == executable.read_bytes()
assert files["etc/ssl/certs/ca-certificates.crt"] == certificates.read_bytes()
for name in ("NOTICE", "certdata.txt"):
    assert files["usr/share/licenses/mozilla-ca/" + name] == (ca_notices / name).read_bytes()
assert b"Source Code Form" in files["usr/share/licenses/mozilla-ca/certdata.txt"]
assert b"https://mozilla.org/MPL/2.0/" in files["usr/share/licenses/mozilla-ca/NOTICE"]
assert b"65532:65532" in files["etc/passwd"]
assert b"65532" in files["etc/group"]
print("Flux OCI image verified: static binary, CA roots and source notices, non-root user, no store links")
