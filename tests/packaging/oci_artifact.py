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
assert len(roots) == 1
root = roots[0]
assert root["mediaType"] == "application/vnd.oci.image.manifest.v1+json"
manifest = json.loads(blob(root))
assert manifest["schemaVersion"] == 2
assert manifest["mediaType"] == root["mediaType"]
assert manifest.get("artifactType") == expected["artifactType"]
assert "subject" not in manifest
assert manifest["annotations"]["org.opencontainers.image.created"] == "1970-01-01T00:00:01Z"

config = blob(manifest["config"])
if expected["config"] is None:
    assert manifest["config"]["mediaType"] == "application/vnd.oci.empty.v1+json"
    assert config == b"{}"
else:
    assert manifest["config"]["mediaType"] == expected["config"]["mediaType"]
    assert config == Path(expected["config"]["file"]).read_bytes()

def verify_layers(manifest, sources):
    assert len(manifest["layers"]) == len(sources)
    for descriptor, source in zip(manifest["layers"], sources):
        assert descriptor["mediaType"] == source["mediaType"]
        assert descriptor["annotations"]["org.opencontainers.image.title"] == source["title"]
        assert blob(descriptor) == Path(source["file"]).read_bytes()


verify_layers(manifest, expected["layers"])

attachments = []
for descriptor in index["manifests"]:
    if descriptor["digest"] == root["digest"]:
        continue
    assert descriptor["mediaType"] == root["mediaType"]
    attachment = json.loads(blob(descriptor))
    assert attachment["schemaVersion"] == 2
    assert attachment["mediaType"] == descriptor["mediaType"]
    subject = attachment["subject"]
    for field in ("digest", "size", "mediaType"):
        assert subject[field] == root[field]
    assert attachment["annotations"]["org.opencontainers.image.created"] == "1970-01-01T00:00:01Z"
    assert attachment["config"]["mediaType"] == "application/vnd.oci.empty.v1+json"
    assert blob(attachment["config"]) == b"{}"
    attachments.append(attachment)

assert len(attachments) == len(expected["attachments"])
for source in expected["attachments"]:
    matches = [
        attachment for attachment in attachments
        if attachment["artifactType"] == source["artifactType"]
        and [layer["digest"] for layer in attachment["layers"]]
        == ["sha256:" + hashlib.sha256(Path(layer["file"]).read_bytes()).hexdigest()
            for layer in source["layers"]]
    ]
    assert len(matches) == 1
    attachment = matches[0]
    verify_layers(attachment, source["layers"])
    attachments.remove(attachment)
assert not attachments

if manifest["config"]["mediaType"] == "application/vnd.wasm.config.v0+json":
    module_config = json.loads(config)
    assert module_config["architecture"] == "wasm"
    assert module_config["os"] == "wasip1"
    assert module_config["layerDigests"] == [layer["digest"] for layer in manifest["layers"]]
    assert len(manifest["layers"]) == 1
    module = blob(manifest["layers"][0])
    assert manifest["layers"][0]["mediaType"] == "application/wasm"
    assert 8 <= len(module) <= 128 << 20
    assert module[:8] == b"\x00asm\x01\x00\x00\x00"

print("OCI distribution artifact verified: digests, payloads, referrers, media types and fixed creation time")
