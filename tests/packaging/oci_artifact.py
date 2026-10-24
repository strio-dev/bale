"""Verify an ORAS-built distribution layout against its immutable inputs."""

import hashlib
import json
from pathlib import Path
import sys


layout, inputs = map(Path, sys.argv[1:])
expected = json.loads(inputs.read_text())
assert json.loads((layout / "oci-layout").read_text()) == {"imageLayoutVersion": "1.0.0"}


def blob(descriptor):
    algorithm, digest = descriptor["digest"].split(":", 1)
    assert algorithm == "sha256"
    raw = (layout / "blobs" / algorithm / digest).read_bytes()
    assert len(raw) == descriptor["size"]
    assert hashlib.sha256(raw).hexdigest() == digest
    return raw


index = json.loads((layout / "index.json").read_bytes())
assert index["schemaVersion"] == 2
roots = [
    descriptor
    for descriptor in index["manifests"]
    if descriptor.get("annotations", {}).get("org.opencontainers.image.ref.name") == "nix"
]
assert len(roots) == len(index["manifests"]) == 1
root = roots[0]
assert root["mediaType"] == "application/vnd.oci.image.manifest.v1+json"
manifest = json.loads(blob(root))
assert manifest["schemaVersion"] == 2
assert manifest["mediaType"] == root["mediaType"]
assert manifest.get("artifactType") == expected["artifactType"]
assert "subject" not in manifest
assert manifest["annotations"]["org.opencontainers.image.created"] == "1970-01-01T00:00:01Z"

assert manifest["config"]["mediaType"] == "application/vnd.oci.empty.v1+json"
assert blob(manifest["config"]) == b"{}"


def verify_layers(manifest, sources):
    assert len(manifest["layers"]) == len(sources)
    for descriptor, source in zip(manifest["layers"], sources):
        assert descriptor["mediaType"] == source["mediaType"]
        assert descriptor["annotations"]["org.opencontainers.image.title"] == source["title"]
        assert blob(descriptor) == Path(source["file"]).read_bytes()


verify_layers(manifest, expected["layers"])

print("OCI distribution artifact verified: digests, payloads, media types and fixed creation time")
