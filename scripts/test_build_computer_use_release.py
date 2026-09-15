"""Tests for deterministic Computer Use release packaging."""

import hashlib
import importlib.util
import json
from pathlib import Path
import stat
import tarfile
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("build-computer-use-release.py")
SPEC = importlib.util.spec_from_file_location("build_computer_use_release", SCRIPT)
builder = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(builder)


class BuildReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name).resolve()
        self.inputs = self.root / "inputs"
        self.output = self.root / "output"
        self.catalog = self.root / "releases.json"
        self.inputs.mkdir()
        for target in builder.TARGETS:
            path = self.inputs / f"metis-cu-{target}"
            path.write_bytes(f"fixture-{target}".encode())
            path.chmod(0o755)

    def tearDown(self):
        self.temp.cleanup()

    def test_builds_two_archives_and_catalog_with_independent_hashes(self):
        releases = builder.build_release(
            self.inputs,
            self.output,
            self.catalog,
            "0.0.1-dev",
            "https://github.com/example/metis/releases/download/v1.0.0",
        )
        self.assertEqual([release["target"] for release in releases], list(builder.TARGETS))
        self.assertEqual(json.loads(self.catalog.read_text())["releases"], releases)
        for release in releases:
            archive = self.output / Path(release["url"]).name
            sidecar = archive.with_name(archive.name + ".sha256")
            self.assertEqual(hashlib.sha256(archive.read_bytes()).hexdigest(), release["sha256"])
            self.assertEqual(sidecar.read_text(), f"{release['sha256']}  {archive.name}\n")
            with tarfile.open(archive, "r:gz") as bundle:
                members = bundle.getmembers()
                self.assertEqual([member.name for member in members], ["metis-cu"])
                self.assertEqual(members[0].mode, 0o755)
                self.assertEqual(members[0].uid, 0)
                self.assertEqual(members[0].gid, 0)
                self.assertEqual(members[0].mtime, 0)
                self.assertEqual(hashlib.sha256(bundle.extractfile(members[0]).read()).hexdigest(), release["binarySha256"])

    def test_same_inputs_produce_same_archives(self):
        first = self.root / "first"
        second = self.root / "second"
        catalog_a = self.root / "a.json"
        catalog_b = self.root / "b.json"
        builder.build_release(self.inputs, first, catalog_a, "1.2.3", "https://example.test/release")
        builder.build_release(self.inputs, second, catalog_b, "1.2.3", "https://example.test/release")
        for target in builder.TARGETS:
            name = builder.archive_name("1.2.3", target)
            self.assertEqual((first / name).read_bytes(), (second / name).read_bytes())
        self.assertEqual(catalog_a.read_bytes(), catalog_b.read_bytes())

    def test_requires_both_executable_targets(self):
        (self.inputs / "metis-cu-darwin-amd64").unlink()
        with self.assertRaises(builder.ReleaseBuildError):
            builder.build_release(self.inputs, self.output, self.catalog, "1.0.0", "https://example.test/release")
        self.assertFalse(self.catalog.exists())

    def test_rejects_unsafe_metadata_and_symlink_binary(self):
        with self.assertRaises(builder.ReleaseBuildError):
            builder.build_release(self.inputs, self.output, self.catalog, "../bad", "https://example.test/release")
        with self.assertRaises(builder.ReleaseBuildError):
            builder.build_release(self.inputs, self.output, self.catalog, "1.0.0", "http://example.test/release")
        real = self.inputs / "real"
        real.write_bytes(b"real")
        real.chmod(stat.S_IRUSR | stat.S_IXUSR)
        target = self.inputs / "metis-cu-darwin-arm64"
        target.unlink()
        target.symlink_to(real)
        with self.assertRaises(builder.ReleaseBuildError):
            builder.build_release(self.inputs, self.output, self.catalog, "1.0.0", "https://example.test/release")


if __name__ == "__main__":
    unittest.main()
