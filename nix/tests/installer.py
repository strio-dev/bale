"""Exercise the real installer without network access or user-directory writes."""

import hashlib
import io
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest

INSTALLER = Path(sys.argv.pop(1)).resolve()
RELEASES = "https://github.com/strio-dev/bale/releases"

MOCK = '''#!{python}
import os, pathlib, shutil, subprocess, sys
name = pathlib.Path(sys.argv[0]).name
if name == "uname":
    print(os.environ["TEST_OS"] if sys.argv[1] == "-s" else os.environ["TEST_ARCH"])
elif name == "id":
    print(os.environ["TEST_UID"])
elif name == "sudo":
    with open(os.environ["TEST_SUDO"], "a") as log:
        log.write(" ".join(sys.argv[1:]) + "\\n")
    if os.environ.get("TEST_SUDO_FAIL"):
        sys.exit(1)
    args = [arg.replace("/usr/local", os.environ["TEST_SYSTEM_PREFIX"]) for arg in sys.argv[1:]]
    sys.exit(subprocess.run(args).returncode)
else:
    url = sys.argv[-1]
    if sys.argv[sys.argv.index("--proto") + 1] != "=https":
        sys.exit(2)
    if sys.argv[sys.argv.index("--proto-redir") + 1] != "=https":
        sys.exit(2)
    with open(os.environ["TEST_REQUESTS"], "a") as log:
        log.write(url + "\\n")
    if "-w" in sys.argv:
        print(os.environ["TEST_LATEST"], end="")
    else:
        if "/download/1.2.3/" not in url:
            sys.exit(22)
        if os.environ.get("TEST_DOWNLOAD_FAIL") in ("all", url.rsplit("/", 1)[1]):
            sys.exit(22)
        source = pathlib.Path(os.environ["TEST_ASSETS"]) / url.rsplit("/", 1)[1]
        shutil.copyfile(source, sys.argv[sys.argv.index("-o") + 1])
'''


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        for name in ("tools", "assets", "home", "tmp", "system"):
            (self.root / name).mkdir()
        for name in ("curl", "uname", "id", "sudo"):
            tool = self.root / "tools" / name
            tool.write_text(MOCK.format(python=sys.executable))
            tool.chmod(0o755)
        self.write_assets()
        self.env = {
            **os.environ,
            "PATH": f"{self.root / 'tools'}:{os.environ['PATH']}",
            "HOME": str(self.root / "home"),
            "TMPDIR": str(self.root / "tmp"),
            "TEST_OS": "Linux",
            "TEST_ARCH": "x86_64",
            "TEST_LATEST": f"{RELEASES}/tag/1.2.3",
            "TEST_ASSETS": str(self.root / "assets"),
            "TEST_REQUESTS": str(self.root / "requests"),
            "TEST_UID": "1000",
            "TEST_SUDO": str(self.root / "sudo-commands"),
            "TEST_SYSTEM_PREFIX": str(self.root / "system"),
        }
        self.env.pop("BALE_INSTALL_PREFIX", None)

    def write_assets(self):
        checksums = []
        for arch in ("amd64", "arm64"):
            asset = self.root / "assets" / f"bale-linux-{arch}.tar.gz"
            entries = [
                ("bale", b"#!/bin/sh\nprintf 'bale fixture\\n'\n", 0o555),
                ("LICENSE", b"MIT license fixture\n", 0o444),
                ("third-party/NOTICE", b"Dependency notices fixture\n", 0o444),
            ]
            with tarfile.open(asset, "w:gz") as archive:
                for name, contents, mode in entries:
                    info = tarfile.TarInfo(name)
                    info.size, info.mode = len(contents), mode
                    archive.addfile(info, io.BytesIO(contents))
            checksums.append(f"{hashlib.sha256(asset.read_bytes()).hexdigest()}  ./{asset.name}\n")
        (self.root / "assets" / "SHA256SUMS").write_text("".join(checksums))

    def run_installer(self, *args, success=True):
        result = subprocess.run(["sh", str(INSTALLER), *args], env=self.env, capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        self.assertEqual(list((self.root / "tmp").iterdir()), [], "temporary files leaked")
        return result

    def test_latest_install_and_repeat(self):
        self.run_installer()
        prefix = self.root / "system"
        self.assertEqual(subprocess.check_output([prefix / "bin/bale"], text=True), "bale fixture\n")
        self.assertEqual((prefix / "share/licenses/bale/LICENSE").read_text(), "MIT license fixture\n")
        self.assertEqual((prefix / "share/licenses/bale/third-party/NOTICE").read_text(), "Dependency notices fixture\n")
        self.assertEqual((self.root / "requests").read_text().splitlines(), [
            f"{RELEASES}/latest", f"{RELEASES}/download/1.2.3/bale-linux-amd64.tar.gz",
            f"{RELEASES}/download/1.2.3/SHA256SUMS",
        ])
        self.run_installer()
        commands = (self.root / "sudo-commands").read_text().splitlines()
        self.assertTrue(commands)
        self.assertTrue(all(command.startswith(("mkdir ", "cp ", "install ")) for command in commands), commands)
        self.assertTrue(all("/usr/local/" in command for command in commands), commands)

    def test_help(self):
        result = self.run_installer("--help")
        self.assertIn("Usage: sh install.sh [VERSION]", result.stdout)
        self.assertFalse((self.root / "requests").exists())
        self.assertEqual(list((self.root / "system").iterdir()), [])

    def test_corrupt_archive_with_valid_checksum(self):
        asset = self.root / "assets/bale-linux-amd64.tar.gz"
        asset.write_bytes(b"not an archive")
        (self.root / "assets/SHA256SUMS").write_text(
            f"{hashlib.sha256(asset.read_bytes()).hexdigest()}  {asset.name}\n"
        )
        self.run_installer(success=False)
        self.assertEqual(list((self.root / "system").iterdir()), [])
        self.assertFalse((self.root / "sudo-commands").exists())

    def test_corrupt_archive_preserves_existing_install(self):
        self.run_installer()
        binary = self.root / "system/bin/bale"
        before = binary.read_bytes()
        commands = (self.root / "sudo-commands").read_text()
        asset = self.root / "assets/bale-linux-amd64.tar.gz"
        asset.write_bytes(b"not an archive")
        (self.root / "assets/SHA256SUMS").write_text(
            f"{hashlib.sha256(asset.read_bytes()).hexdigest()}  {asset.name}\n"
        )
        self.run_installer(success=False)
        self.assertEqual(binary.read_bytes(), before)
        self.assertEqual((self.root / "sudo-commands").read_text(), commands)

    def test_failed_download_preserves_existing_install(self):
        self.run_installer()
        binary = self.root / "system/bin/bale"
        before = binary.read_bytes()
        commands = (self.root / "sudo-commands").read_text()
        for asset in ("bale-linux-amd64.tar.gz", "SHA256SUMS"):
            with self.subTest(asset=asset):
                self.env["TEST_DOWNLOAD_FAIL"] = asset
                self.run_installer(success=False)
                self.assertEqual(binary.read_bytes(), before)
                self.assertEqual((self.root / "sudo-commands").read_text(), commands)

    def test_missing_required_tool(self):
        tools = self.root / "tools"
        for name in ("sh", "tar", "awk", "mktemp", "install"):
            (tools / name).symlink_to(shutil.which(name))
        self.env["PATH"] = str(tools)
        result = self.run_installer(success=False)
        self.assertIn("Required tool not found: sha256sum", result.stderr)
        self.assertFalse((self.root / "requests").exists())
        self.assertEqual(list((self.root / "system").iterdir()), [])

    def test_invalid_release_arguments(self):
        for args in (("",), ("latest/path",), ("1.2.3", "extra")):
            with self.subTest(args=args):
                self.run_installer(*args, success=False)
        self.assertFalse((self.root / "requests").exists())
        self.assertEqual(list((self.root / "system").iterdir()), [])

    def test_missing_sudo(self):
        tools = self.root / "tools"
        (tools / "sudo").unlink()
        for name in ("sh", "tar", "sha256sum", "awk", "mktemp", "install"):
            (tools / name).symlink_to(shutil.which(name))
        self.env["PATH"] = str(tools)
        result = self.run_installer(success=False)
        self.assertIn("requires sudo", result.stderr)
        self.assertEqual(list((self.root / "system").iterdir()), [])
        self.assertEqual(list((self.root / "home").iterdir()), [])

    def test_sudo_failure_preserves_existing_install(self):
        self.run_installer()
        binary = self.root / "system/bin/bale"
        before = binary.read_bytes()
        self.env["TEST_SUDO_FAIL"] = "1"
        self.run_installer(success=False)
        self.assertEqual(binary.read_bytes(), before)
        self.assertEqual(list((self.root / "home").iterdir()), [])

    def test_exact_version_arm64_custom_prefix(self):
        self.env.update(TEST_ARCH="aarch64", BALE_INSTALL_PREFIX=str(self.root / "custom prefix"))
        self.run_installer("1.2.3")
        self.assertTrue((self.root / "custom prefix/bin/bale").is_file())
        self.assertIn("bale-linux-arm64.tar.gz", (self.root / "requests").read_text())
        self.assertNotIn("/latest", (self.root / "requests").read_text())
        self.assertFalse((self.root / "sudo-commands").exists())

    def test_custom_prefix_already_on_path(self):
        prefix = self.root / "home/.local"
        self.env.update(BALE_INSTALL_PREFIX=str(prefix), PATH=f"{prefix / 'bin'}:{self.env['PATH']}")
        result = self.run_installer()
        self.assertEqual(subprocess.check_output(["bale"], env=self.env, text=True), "bale fixture\n")
        self.assertNotIn("PATH", result.stdout)
        self.assertFalse((self.root / "sudo-commands").exists())
        self.assertEqual(list((self.root / "home").iterdir()), [prefix])

    def test_bad_checksum_preserves_existing_install(self):
        self.run_installer()
        binary = self.root / "system/bin/bale"
        before = binary.read_bytes()
        commands = (self.root / "sudo-commands").read_text()
        (self.root / "assets/bale-linux-amd64.tar.gz").write_bytes(b"corrupted download")
        self.run_installer(success=False)
        self.assertEqual(binary.read_bytes(), before)
        self.assertEqual((self.root / "sudo-commands").read_text(), commands)

    def test_missing_checksum(self):
        (self.root / "assets/SHA256SUMS").write_text("")
        self.assertIn("Missing checksum", self.run_installer(success=False).stderr)
        self.assertEqual(list((self.root / "system").iterdir()), [])
        self.assertFalse((self.root / "sudo-commands").exists())

    def test_no_release_and_unsupported_platform(self):
        self.env["TEST_LATEST"] = RELEASES
        self.assertIn("No published release", self.run_installer(success=False).stderr)
        self.env["TEST_OS"] = "Darwin"
        self.assertIn("requires Linux", self.run_installer(success=False).stderr)
        self.env.update(TEST_OS="Linux", TEST_ARCH="riscv64")
        self.assertIn("Supported architectures", self.run_installer(success=False).stderr)


if __name__ == "__main__":
    unittest.main()
