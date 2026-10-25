"""Check release preflight and output isolation without publishing artifacts."""

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest


STAGE = Path(sys.argv.pop(1)).resolve()
PUBLISH = Path(sys.argv.pop(1)).resolve()
DIGEST = "sha256:" + "a" * 64
INPUTS = [
    "flux-amd64.digest",
    "flux-arm64.digest",
    "helm-consumer-amd64.digest",
    "helm-consumer-arm64.digest",
    "helm-discovery.digest",
]


class IntegrationReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.env = {
            **os.environ,
            "HOME": str(self.root / "home"),
            "REGCTL_CONFIG": str(self.root / "regctl.json"),
        }

    def run_helper(self, helper, *args):
        return subprocess.run(
            [helper, *args], env=self.env, capture_output=True, text=True, timeout=10
        )

    def write_digests(self):
        directory = self.root / "digests"
        directory.mkdir()
        for name in INPUTS:
            (directory / name).write_text(DIGEST + "\n")
        return directory

    def registry(self, missing_discovery=False):
        requests = []

        class Handler(BaseHTTPRequestHandler):
            def do_HEAD(self):
                requests.append((self.command, self.path))
                if missing_discovery and "/plugins/helm-discovery/" not in self.path:
                    self.send_response(200)
                    self.send_header("Content-Type", "application/vnd.oci.image.manifest.v1+json")
                    self.send_header("Content-Length", "2")
                    self.send_header("Docker-Content-Digest", DIGEST)
                else:
                    self.send_response(404 if missing_discovery else 401)
                self.end_headers()

            def do_POST(self):
                requests.append((self.command, self.path))
                self.send_response(500)
                self.end_headers()

            do_PUT = do_POST

            def log_message(self, *_args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever)
        thread.start()

        def cleanup():
            server.shutdown()
            thread.join()
            server.server_close()

        self.addCleanup(cleanup)
        return f"127.0.0.1:{server.server_port}/bale", requests

    def assert_failed_without_output(self, result):
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout, "", result.stdout)

    def test_usage_and_plain_http_usage(self):
        for helper in (STAGE, PUBLISH):
            for args in ((), ("--plain-http",)):
                with self.subTest(helper=helper.name, args=args):
                    result = self.run_helper(helper, *args)
                    self.assert_failed_without_output(result)
                    self.assertIn("Usage:", result.stderr)

    def test_stage_rejects_bad_repository_before_creating_output(self):
        output = self.root / "output"
        for repository in (
            "https://registry.invalid/bale",
            "registry.invalid/bale:version",
            "registry.invalid/bale@" + DIGEST,
            "registry.invalid/org/../bale",
            "registry.invalid/org//bale",
            "registry.invalid/bale/",
        ):
            with self.subTest(repository=repository):
                result = self.run_helper(STAGE, repository, "123", output)
                self.assert_failed_without_output(result)
                self.assertFalse(output.exists())

    def test_stage_rejects_bad_run_key_before_creating_output(self):
        output = self.root / "output"
        for key in ("", "../escape", "two words", "x" * 82):
            with self.subTest(key=key):
                result = self.run_helper(STAGE, "registry.invalid/bale", key, output)
                self.assert_failed_without_output(result)
                self.assertFalse(output.exists())

    def test_stage_preserves_existing_output(self):
        output = self.root / "output"
        output.mkdir()
        marker = output / "flux-amd64.digest"
        marker.write_text("retained")
        result = self.run_helper(STAGE, "registry.invalid/bale", "123", output)
        self.assert_failed_without_output(result)
        self.assertEqual(marker.read_text(), "retained")
        self.assertEqual(list(output.iterdir()), [marker])

    def test_stage_plain_http_reaches_registry_and_cleans_failed_output(self):
        repository, requests = self.registry()
        output = self.root / "output"
        result = self.run_helper(STAGE, "--plain-http", repository, "123", output)
        self.assert_failed_without_output(result)
        self.assertTrue(requests, result.stderr)
        self.assertNotIn("unknown flag", result.stderr)
        self.assertFalse(output.exists())

    def test_publish_requires_all_five_digests_before_network_access(self):
        for missing in INPUTS:
            with self.subTest(missing=missing):
                directory = self.write_digests()
                (directory / missing).unlink()
                result = self.run_helper(PUBLISH, "registry.invalid/bale", directory)
                self.assert_failed_without_output(result)
                self.assertIn(missing, result.stderr)
                for entry in directory.iterdir():
                    entry.unlink()
                directory.rmdir()

    def test_publish_rejects_malformed_digests_before_network_access(self):
        directory = self.write_digests()
        for invalid in (
            "",
            "sha256:" + "z" * 64,
            "sha256:" + "a" * 63,
            "sha256:" + "a" * 65,
            DIGEST + "\n" + DIGEST,
        ):
            with self.subTest(invalid=invalid):
                # Corrupt the final required input to prove the complete set is
                # checked before the first registry operation can begin.
                (directory / INPUTS[-1]).write_text(invalid)
                result = self.run_helper(PUBLISH, "registry.invalid/bale", directory)
                self.assert_failed_without_output(result)
                self.assertIn("Expected a SHA-256 artifact digest", result.stderr)

    def test_publish_checks_missing_remote_artifact_before_promoting(self):
        repository, requests = self.registry(missing_discovery=True)
        directory = self.write_digests()
        result = self.run_helper(PUBLISH, "--plain-http", repository, directory)
        self.assert_failed_without_output(result)
        self.assertTrue(any("/plugins/helm-discovery/" in path for _, path in requests), result.stderr)
        self.assertTrue(all(method == "HEAD" for method, _ in requests), requests)
        self.assertFalse(any(path.rsplit("/", 1)[-1] == "dev" for _, path in requests), requests)


if __name__ == "__main__":
    unittest.main()
