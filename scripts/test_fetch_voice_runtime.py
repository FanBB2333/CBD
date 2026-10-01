"""Offline checks for the fixed-hash runtime downloader; no modem access."""
import hashlib
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("voice_runtime", Path(__file__).with_name("fetch-voice-runtime.py"))
runtime = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runtime)


class RuntimeTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.dest = Path(self.directory.name) / "runtime"
        self.payloads = {"first": b"verified first", "second": b"verified second"}
        manifest = {name: (hashlib.sha256(data).hexdigest(), 0o644) for name, data in self.payloads.items()}
        self.manifest = patch.object(runtime, "FILES", manifest)
        self.manifest.start()
        self.addCleanup(self.manifest.stop)

    def download(self, url):
        return self.payloads[url.rsplit("/", 1)[-1]]

    def test_download_verify_and_reuse_without_network(self):
        runtime.fetch(self.dest, self.download)
        runtime.verify(self.dest)
        def offline(_):
            self.fail("verified local artifact was downloaded again")
        runtime.fetch(self.dest, offline)
        (self.dest / "first").write_bytes(b"corrupt")
        with self.assertRaisesRegex(ValueError, "SHA-256 mismatch"):
            runtime.verify(self.dest)

    def test_bad_download_keeps_all_installed_files(self):
        self.dest.mkdir(mode=0o750)
        for name in self.payloads:
            (self.dest / name).write_bytes(b"old")
        def corrupt_second(url):
            return b"tampered" if url.endswith("second") else self.download(url)
        with self.assertRaisesRegex(ValueError, "download SHA-256 mismatch"):
            runtime.fetch(self.dest, corrupt_second)
        for name in self.payloads:
            self.assertEqual((self.dest / name).read_bytes(), b"old")

    def test_symlinks_and_writable_artifacts_are_rejected(self):
        runtime.fetch(self.dest, self.download)
        (self.dest / "first").chmod(0o666)
        with self.assertRaisesRegex(ValueError, "regular file"):
            runtime.verify(self.dest)
        (self.dest / "first").unlink()
        (self.dest / "first").symlink_to(self.dest / "second")
        with self.assertRaisesRegex(ValueError, "symlink"):
            runtime.fetch(self.dest, self.download)


class ManifestTests(unittest.TestCase):
    def test_manifest_matches_gateway_allowlist(self):
        source = Path(__file__).resolve().parents[1] / "gateway/internal/voice/qdc507/audio.go"
        code = source.read_text()
        for name, (digest, _) in runtime.FILES.items():
            self.assertIn(f'name: "{name}"', code)
            self.assertIn(f'hash: "{digest}"', code)


if __name__ == "__main__":
    unittest.main()
