"""Offline tar-fixture checks; no helper is executed and no network is used."""

import contextlib
import copy
import gzip
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest import mock
import urllib.request


SCRIPT = Path(__file__).with_name("bundle-computer-use.py")
SPEC = importlib.util.spec_from_file_location("bundle_computer_use", SCRIPT)
bundler = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(bundler)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def tar_bytes(entries):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w:gz", format=tarfile.USTAR_FORMAT) as archive:
        for name, data, mode, kind in entries:
            member = tarfile.TarInfo(name)
            member.size = len(data)
            member.mode = mode
            member.type = kind
            if kind in (tarfile.SYMTYPE, tarfile.LNKTYPE):
                member.linkname = "outside"
            archive.addfile(member, io.BytesIO(data))
    return output.getvalue()


class BundleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        # macOS /var is a system symlink; use the canonical fixture root.
        self.root = Path(self.temp.name).resolve()
        self.app = self.root / "METIS.app"
        self.resources = self.app / "Contents/Resources"
        self.resources.mkdir(parents=True)
        self.artifacts = self.root / "artifacts"
        self.artifacts.mkdir()
        self.catalog_path = self.root / "releases.json"
        self.catalog = {"releases": []}
        self.payloads = {}
        for target in bundler.TARGETS:
            # Deliberately inert bytes: bundling only inspects archive content.
            binary = ("fixture for " + target).encode()
            artifact = tar_bytes([("metis-cu", binary, 0o755, tarfile.REGTYPE)])
            name = "metis-cu_1.2.3_" + target.replace("-", "_") + ".tar.gz"
            release = {
                "version": "1.2.3", "protocolVersion": 1, "target": target,
                "url": "https://example.test/releases/" + name,
                "sha256": digest(artifact), "binarySha256": digest(binary),
                "format": "tar.gz", "binaryPath": "metis-cu", "size": len(artifact),
            }
            self.catalog["releases"].append(release)
            (self.artifacts / name).write_bytes(artifact)
            self.payloads[target] = binary
        self.write_catalog()
        patch = mock.patch.object(bundler, "CATALOG", self.catalog_path)
        patch.start()
        self.addCleanup(patch.stop)
        network = mock.patch.object(urllib.request.OpenerDirector, "open", side_effect=AssertionError("network must not be used"))
        network.start()
        self.addCleanup(network.stop)

    def write_catalog(self):
        self.catalog_path.write_text(json.dumps(self.catalog))

    def bundle(self, **kwargs):
        self.write_catalog()
        return bundler.bundle_app(self.app, self.artifacts, **kwargs)

    def replace_archive(self, data):
        release = self.catalog["releases"][0]
        (self.artifacts / bundler.artifact_name(release)).write_bytes(data)
        release.update(sha256=digest(data), size=len(data))

    def assert_clean_failure(self):
        self.assertFalse((self.resources / "computer-use").exists())
        self.assertEqual(list(self.resources.glob(".computer-use-stage-*")), [])

    def test_bundles_both_pinned_archives_unchanged_without_execution(self):
        result = self.bundle()
        self.assertEqual(len(result), 2)
        self.assertEqual({path.name for path in result}, {bundler.artifact_name(r) for r in self.catalog["releases"]})
        for path in result:
            self.assertEqual(path.read_bytes(), (self.artifacts / path.name).read_bytes())
            self.assertEqual(path.stat().st_mode & 0o777, 0o644)
        self.assertEqual((self.resources / "computer-use").stat().st_mode & 0o777, 0o755)
        self.assertEqual(list(self.resources.glob(".computer-use-stage-*")), [])

    def test_catalog_order_matches_runtime_first_compatible_selection(self):
        old = copy.deepcopy(self.catalog["releases"][0])
        old["version"] = "0.1.0"
        selected = bundler.select_releases({"releases": [old] + self.catalog["releases"]})
        self.assertEqual(selected[0]["version"], "0.1.0")

    def test_empty_catalog_fails_production_and_explicitly_skips_development(self):
        self.catalog["releases"] = []
        with self.assertRaisesRegex(bundler.BundleError, "catalog is empty"):
            self.bundle()
        self.assertEqual(self.bundle(allow_empty=True), [])
        self.assert_clean_failure()

    def test_allow_empty_does_not_allow_partial_catalog(self):
        self.catalog["releases"].pop()
        with self.assertRaisesRegex(bundler.BundleError, "darwin-amd64"):
            self.bundle(allow_empty=True)
        self.assert_clean_failure()

    def test_nonmatching_protocol_does_not_count_as_supported_target(self):
        self.catalog["releases"][0]["protocolVersion"] = 2
        with self.assertRaisesRegex(bundler.BundleError, "darwin-arm64"):
            self.bundle()

    def test_invalid_catalog_fields_are_rejected(self):
        for key, value in (("sha256", "A" * 64), ("binarySha256", ""), ("size", -1), ("size", True), ("size", bundler.MAX_BYTES + 1), ("format", "zip"), ("version", ""), ("url", None), ("binaryPath", "../metis-cu"), ("binaryPath", "/metis-cu"), ("binaryPath", "bin//metis-cu"), ("binaryPath", "metis-other")):
            with self.subTest(key=key, value=value):
                catalog = copy.deepcopy(self.catalog)
                catalog["releases"][0][key] = value
                with self.assertRaises(bundler.BundleError):
                    bundler.select_releases(catalog)

    def test_duplicate_json_keys_are_rejected(self):
        self.catalog_path.write_text('{"releases":[],"releases":[]}')
        with self.assertRaisesRegex(bundler.BundleError, "duplicate catalog key"):
            bundler.load_catalog()

    def test_unsafe_urls_and_filename_collisions_are_rejected(self):
        for url in ("http://example.test/a.tar.gz", "https://u:p@example.test/a.tar.gz", "https://example.test/a.tar.gz#fragment", "https://example.test/a%2fb.tar.gz", "https://example.test/a.tar.gz\n", "https://example.test:bad/a.tar.gz", "https://[broken/a.tar.gz"):
            with self.subTest(url=url):
                catalog = copy.deepcopy(self.catalog)
                catalog["releases"][0]["url"] = url
                with self.assertRaises(bundler.BundleError):
                    bundler.select_releases(catalog)
        self.catalog["releases"][1]["url"] = self.catalog["releases"][0]["url"]
        with self.assertRaisesRegex(bundler.BundleError, "basenames must be distinct"):
            self.bundle()

    def test_archive_hash_size_and_binary_hash_are_independent_pins(self):
        for field, value, message in (("sha256", "0" * 64, "archive SHA256"), ("size", 1, "pinned size"), ("binarySha256", "0" * 64, "binary SHA256")):
            with self.subTest(field=field):
                original = self.catalog["releases"][0][field]
                self.catalog["releases"][0][field] = value
                with self.assertRaisesRegex(bundler.BundleError, message):
                    self.bundle()
                self.assert_clean_failure()
                self.catalog["releases"][0][field] = original

    def test_rejects_links_devices_extra_members_wrong_path_and_mode(self):
        binary = self.payloads[bundler.TARGETS[0]]
        valid = ("metis-cu", binary, 0o755, tarfile.REGTYPE)
        cases = [
            [("metis-cu", b"", 0o755, tarfile.SYMTYPE)],
            [("metis-cu", b"", 0o755, tarfile.LNKTYPE)],
            [("metis-cu", b"", 0o755, tarfile.FIFOTYPE)],
            [("metis-cu", binary, 0o4755, tarfile.REGTYPE)],
            [("metis-cu", binary, 0o644, tarfile.REGTYPE)],
            [("../metis-cu", binary, 0o755, tarfile.REGTYPE)],
            [("other", binary, 0o755, tarfile.REGTYPE)],
            [valid, valid],
            [valid, ("resource", b"data", 0o644, tarfile.REGTYPE)],
        ]
        for entries in cases:
            with self.subTest(entries=entries):
                self.replace_archive(tar_bytes(entries))
                with self.assertRaises(bundler.BundleError):
                    self.bundle()
                self.assert_clean_failure()

    def test_nested_binary_path_is_supported_without_directory_entries(self):
        binary = self.payloads[bundler.TARGETS[0]]
        self.catalog["releases"][0]["binaryPath"] = "bundle/bin/metis-cu"
        self.replace_archive(tar_bytes([("bundle/bin/metis-cu", binary, 0o755, tarfile.REGTYPE)]))
        self.assertEqual(len(self.bundle()), 2)

    def test_truncated_gzip_and_trailing_tar_payload_are_rejected(self):
        original = (self.artifacts / bundler.artifact_name(self.catalog["releases"][0])).read_bytes()
        for data in (original[:-8], gzip.compress(gzip.decompress(original) + b"hidden payload")):
            with self.subTest(size=len(data)):
                self.replace_archive(data)
                with self.assertRaises((bundler.BundleError, EOFError, tarfile.TarError)):
                    self.bundle()
                self.assert_clean_failure()

    def test_archive_and_decompression_size_limits(self):
        release = self.catalog["releases"][0]
        with mock.patch.object(bundler, "MAX_BYTES", 32):
            with self.assertRaisesRegex(bundler.BundleError, "size limit"):
                bundler.copy_verified(io.BytesIO(b"x" * 33), self.root / "copy", release)
        binary = b"x" * 20000
        self.replace_archive(tar_bytes([("metis-cu", binary, 0o755, tarfile.REGTYPE)]))
        self.catalog["releases"][0]["binarySha256"] = digest(binary)
        with mock.patch.object(bundler, "MAX_BYTES", 500):
            with self.assertRaisesRegex(bundler.BundleError, "decompressed size"):
                self.bundle()
        self.assert_clean_failure()

    def test_failure_on_second_architecture_publishes_nothing(self):
        self.catalog["releases"][1]["binarySha256"] = "0" * 64
        with self.assertRaisesRegex(bundler.BundleError, "binary SHA256"):
            self.bundle()
        self.assert_clean_failure()

    def test_existing_output_is_never_overwritten(self):
        destination = self.resources / "computer-use"
        destination.mkdir()
        with self.assertRaisesRegex(bundler.BundleError, "already exists"):
            self.bundle()
        self.assertEqual(list(destination.iterdir()), [])
        stage = self.resources / "stage"
        stage.mkdir()
        (stage / "sentinel").write_text("preserve")
        with self.assertRaises(OSError):
            bundler.publish_stage(stage, destination)
        self.assertEqual((stage / "sentinel").read_text(), "preserve")
        self.assertEqual(list(destination.iterdir()), [])

    def test_symlink_archive_catalog_resources_and_output_are_rejected(self):
        archive = self.artifacts / bundler.artifact_name(self.catalog["releases"][0])
        actual = archive.with_suffix(".original")
        archive.rename(actual)
        archive.symlink_to(actual)
        with self.assertRaisesRegex(bundler.BundleError, "symlink"):
            self.bundle()
        self.assert_clean_failure()
        archive.unlink()
        actual.rename(archive)
        catalog = self.catalog_path.with_suffix(".original")
        self.catalog_path.rename(catalog)
        self.catalog_path.symlink_to(catalog)
        with self.assertRaisesRegex(bundler.BundleError, "symlink"):
            bundler.bundle_app(self.app, self.artifacts)
        self.catalog_path.unlink()
        catalog.rename(self.catalog_path)
        destination = self.resources / "computer-use"
        destination.symlink_to(self.root / "missing", target_is_directory=True)
        with self.assertRaisesRegex(bundler.BundleError, "already exists"):
            self.bundle()
        destination.unlink()
        actual_resources = self.root / "resources"
        self.resources.rename(actual_resources)
        self.resources.symlink_to(actual_resources, target_is_directory=True)
        with self.assertRaisesRegex(bundler.BundleError, "symlink"):
            self.bundle()

    def test_redirects_must_remain_https_and_are_bounded(self):
        handler = bundler.SafeRedirects(float("inf"))
        request = urllib.request.Request("https://example.test/archive.tar.gz")
        for url in ("http://example.test/a", "https://user@example.test/a", "file:///tmp/a"):
            with self.subTest(url=url), self.assertRaises(bundler.BundleError):
                handler.redirect_request(request, None, 302, "Found", {}, url)
        for _ in range(5):
            redirected = handler.redirect_request(request, None, 302, "Found", {}, "https://cdn.example.test/archive.tar.gz?signature=fixture")
            self.assertEqual(redirected.type, "https")
        with self.assertRaisesRegex(bundler.BundleError, "redirect count"):
            handler.redirect_request(request, None, 302, "Found", {}, "https://cdn.example.test/a")

    def test_expired_download_deadline_is_checked_without_network(self):
        with self.assertRaisesRegex(bundler.BundleError, "deadline"):
            bundler.copy_verified(io.BytesIO(b"bytes"), self.root / "expired", self.catalog["releases"][0], deadline=0)

    def test_https_download_path_verifies_fixture_bytes_and_sets_timeout(self):
        release = self.catalog["releases"][0]
        data = (self.artifacts / bundler.artifact_name(release)).read_bytes()
        response = io.BytesIO(data)
        response.status = 200
        response.headers = {"Content-Length": str(len(data))}
        response.geturl = lambda: release["url"]
        opener = mock.Mock()
        opener.open.return_value = response
        with mock.patch.object(urllib.request, "build_opener", return_value=opener):
            destination = self.root / "mock-download"
            bundler.obtain_archive(release, destination, None)
        self.assertEqual(destination.read_bytes(), data)
        self.assertEqual(opener.open.call_args.kwargs["timeout"], bundler.SOCKET_SECONDS)

    def test_https_response_guards_before_copying_any_payload(self):
        release = self.catalog["releases"][0]
        for status, headers, final_url in (
            (404, {}, release["url"]),
            (200, {"Content-Encoding": "gzip"}, release["url"]),
            (200, {"Content-Length": str(bundler.MAX_BYTES + 1)}, release["url"]),
            (200, {}, "http://example.test/archive.tar.gz"),
        ):
            with self.subTest(status=status, headers=headers, final_url=final_url):
                response = io.BytesIO(b"unused")
                response.status, response.headers = status, headers
                response.geturl = lambda: final_url
                opener = mock.Mock()
                opener.open.return_value = response
                destination = self.root / "rejected-download"
                with mock.patch.object(urllib.request, "build_opener", return_value=opener):
                    with self.assertRaises(bundler.BundleError):
                        bundler.obtain_archive(release, destination, None)
                self.assertFalse(destination.exists())

    def test_cli_empty_skip_and_failure_messages(self):
        self.catalog["releases"] = []
        self.write_catalog()
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            self.assertEqual(bundler.main(["--app", str(self.app)]), 1)
            self.assertEqual(bundler.main(["--app", str(self.app), "--allow-empty"]), 0)
        self.assertIn("production", stderr.getvalue())
        self.assertIn("SKIPPED", stdout.getvalue())

    def test_protocol_constant_matches_compiled_runtime(self):
        source = (SCRIPT.parent.parent / "internal/computeruse/types.go").read_text()
        self.assertIn(f"const ProtocolVersion = {bundler.PROTOCOL_VERSION}\n", source)


if __name__ == "__main__":
    unittest.main()
