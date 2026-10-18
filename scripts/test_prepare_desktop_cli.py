"""Offline fixtures, including a tiny Go build: no product CLI/app or model calls."""
import argparse
import hashlib
import importlib.util
import json
import os
import shutil
import subprocess
from pathlib import Path
import plistlib
import struct
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('desktop_cli', Path(__file__).with_name('prepare-desktop-cli.py'))
bundle = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bundle)


def macho(arch):
    return struct.pack('<IIII', 0xfeedfacf, {'amd64': 0x01000007, 'arm64': 0x0100000C}[arch], 0, 2) + b'synthetic-only'


class DesktopPayloadTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        (self.root / 'metis-desktop').mkdir()
        (self.root / 'VERSION').write_text('1.2.3\n')
        (self.root / 'metis-desktop/wails.json').write_text(json.dumps({'info': {'productVersion': '1.2.3'}}))
        self.app = self.root / 'METIS.app'
        (self.app / 'Contents').mkdir(parents=True)
        (self.app / 'Contents/Info.plist').write_bytes(plistlib.dumps({'CFBundleShortVersionString': '1.2.3'}))
        self.cli = self.root / 'fresh-metis'
        self.cli.write_bytes(macho('arm64'))
        self.cli.chmod(0o755)

    def args(self, **kwargs):
        result = argparse.Namespace(repo=self.root, platform='darwin/arm64', app=self.app,
                                  output=None, cli=self.cli, go='synthetic-go', sign_identity='-',
                                  commit='test-source', date='2026-10-17T00:00:00Z',
                                  source_manifest_sha256='a' * 64, verify_only=False,
                                  execute_version=False)
        for key, value in kwargs.items():
            setattr(result, key, value)
        return result

    def fake_run(self, argv, **kwargs):
        if argv[:3] == ['synthetic-go', 'version', '-m']:
            return 'binary: go1.26\n\tbuild\t-ldflags="-X ' + bundle.VERSION_PACKAGE + '.Version=1.2.3 -X ' + bundle.VERSION_PACKAGE + '.Commit=test-source -X ' + bundle.VERSION_PACKAGE + '.Date=2026-10-17T00:00:00Z"\n'
        if argv[:2] == ['codesign', '--force']:
            # Signing modifies bytes; the stored hash must describe these bytes.
            Path(argv[-1]).write_bytes(Path(argv[-1]).read_bytes() + b'signed-fixture')
            return ''
        if argv[:2] == ['codesign', '--verify']:
            return ''
        self.fail(f'unexpected synthetic command: {argv}')

    def prepared(self):
        with patch.object(bundle, 'run', side_effect=self.fake_run):
            return bundle.prepare(self.args())

    def test_missing_payload_fails(self):
        with self.assertRaisesRegex(bundle.BundleError, 'manifest is missing'):
            bundle.verify_payload(self.app / 'Contents/Resources/bin', 'darwin/arm64', '1.2.3', mac=True)

    def test_manifest_hash_describes_signed_cli(self):
        manifest = self.prepared()
        payload = self.app / 'Contents/Resources/bin/metis'
        self.assertTrue(payload.read_bytes().endswith(b'signed-fixture'))
        self.assertEqual(manifest['sha256'], hashlib.sha256(payload.read_bytes()).hexdigest())
        self.assertEqual(manifest['size_bytes'], payload.stat().st_size)
        self.assertEqual(manifest['source_manifest_sha256'], 'a' * 64)

    def test_tampered_payload_fails(self):
        self.prepared()
        path = self.app / 'Contents/Resources/bin/metis'
        path.write_bytes(path.read_bytes() + b'tampered')
        with self.assertRaisesRegex(bundle.BundleError, 'hash or size'):
            bundle.verify_payload(path.parent, 'darwin/arm64', '1.2.3', mac=True)

    def test_wrong_architecture_fails_before_signing(self):
        self.cli.write_bytes(macho('amd64'))
        with patch.object(bundle, 'run') as run:
            with self.assertRaisesRegex(bundle.BundleError, 'architecture differs'):
                bundle.prepare(self.args())
        run.assert_not_called()

    def test_mismatched_app_version_fails(self):
        (self.app / 'Contents/Info.plist').write_bytes(plistlib.dumps({'CFBundleShortVersionString': '0.0.1'}))
        with self.assertRaisesRegex(bundle.BundleError, 'CFBundleShortVersionString'):
            bundle.prepare(self.args())

    def test_mismatched_wails_version_fails(self):
        (self.root / 'metis-desktop/wails.json').write_text('{"info":{"productVersion":"0.0.1"}}')
        with self.assertRaisesRegex(bundle.BundleError, 'productVersion'):
            bundle.prepare(self.args())

    def test_mismatched_actual_cli_build_version_fails(self):
        with patch.object(bundle, 'run', return_value='binary: go1.26\n\tbuild\t-ldflags="-X ' + bundle.VERSION_PACKAGE + '.Version=0.0.1"\n'):
            with self.assertRaisesRegex(bundle.BundleError, 'Go build version'):
                bundle.prepare(self.args())

    def test_same_version_older_source_cli_is_rejected(self):
        text = self.fake_run(['synthetic-go', 'version', '-m', self.cli]).replace('Commit=test-source', 'Commit=older-source')
        with patch.object(bundle, 'run', return_value=text):
            with self.assertRaisesRegex(bundle.BundleError, 'build Commit'):
                bundle.prepare(self.args())

    def test_reused_cli_without_commit_flag_must_match_checkout_revision(self):
        def commands(argv, **kwargs):
            if argv[:3] == ['git', 'rev-parse', 'HEAD']:
                return 'current-source\n'
            return self.fake_run(argv, **kwargs)
        with patch.object(bundle, 'run', side_effect=commands):
            with self.assertRaisesRegex(bundle.BundleError, 'build Commit'):
                bundle.prepare(self.args(commit=None, date=None))

    def test_reused_cli_without_date_flag_preserves_actual_build_date(self):
        actual_date = '2020-01-02T03:04:05Z'
        def commands(argv, **kwargs):
            if argv[:3] == ['git', 'rev-parse', 'HEAD']:
                return 'test-source\n'
            result = self.fake_run(argv, **kwargs)
            return result.replace('2026-10-17T00:00:00Z', actual_date)
        with patch.object(bundle, 'run', side_effect=commands):
            manifest = bundle.prepare(self.args(commit=None, date=None))
        self.assertEqual(manifest['commit'], 'test-source')
        self.assertEqual(manifest['built_at'], actual_date)

    def test_reused_cli_without_date_metadata_cannot_invent_timestamp(self):
        def commands(argv, **kwargs):
            text = self.fake_run(argv, **kwargs)
            return text.replace(' -X ' + bundle.VERSION_PACKAGE + '.Date=2026-10-17T00:00:00Z', '')
        with patch.object(bundle, 'run', side_effect=commands):
            with self.assertRaisesRegex(bundle.BundleError, 'build Date'):
                bundle.prepare(self.args(date=None))

    def test_reused_cli_with_invalid_actual_date_fails(self):
        def commands(argv, **kwargs):
            return self.fake_run(argv, **kwargs).replace('2026-10-17T00:00:00Z', 'unknown')
        with patch.object(bundle, 'run', side_effect=commands):
            with self.assertRaisesRegex(bundle.BundleError, 'build Date'):
                bundle.prepare(self.args(date=None))

    def test_reused_universal_slices_cannot_disagree_on_build_date(self):
        header = bytearray(48)
        struct.pack_into('>II', header, 0, 0xcafebabe, 2)
        struct.pack_into('>I', header, 8, 0x01000007)
        struct.pack_into('>I', header, 28, 0x0100000C)
        self.cli.write_bytes(header)
        def commands(argv, **kwargs):
            if argv[0] == 'lipo':
                Path(argv[-1]).write_bytes(macho(Path(argv[-1]).name))
                return ''
            text = self.fake_run(argv, **kwargs)
            if argv[:3] == ['synthetic-go', 'version', '-m'] and Path(argv[-1]).name == 'amd64':
                text = text.replace('2026-10-17T00:00:00Z', '2020-01-02T03:04:05Z')
            return text
        with patch.object(bundle, 'run', side_effect=commands):
            with self.assertRaisesRegex(bundle.BundleError, 'build Date'):
                bundle.prepare(self.args(platform='darwin/universal', date=None))

    def test_same_source_self_build_still_uses_readonly_target_flags(self):
        builds = []
        def commands(argv, **kwargs):
            if argv[:2] == ['synthetic-go', 'build']:
                builds.append((argv, kwargs))
                Path(argv[argv.index('-o') + 1]).write_bytes(macho(kwargs['env']['GOARCH']))
                return ''
            return self.fake_run(argv, **kwargs)
        with patch.object(bundle, 'run', side_effect=commands):
            manifest = bundle.prepare(self.args(cli=None))
        self.assertEqual(manifest['commit'], 'test-source')
        self.assertEqual(manifest['built_at'], '2026-10-17T00:00:00Z')
        self.assertEqual(len(builds), 1)
        argv, kwargs = builds[0]
        self.assertIn('-mod=readonly', argv)
        self.assertIn('-trimpath', argv)
        self.assertEqual(argv[argv.index('-tags') + 1], bundle.runtime_build_tag(
            '1.2.3', 'test-source', '2026-10-17T00:00:00Z'))
        self.assertEqual(kwargs['env']['CGO_ENABLED'], '0')
        self.assertEqual(kwargs['env']['GOOS'], 'darwin')

    def test_real_trimpath_buildinfo_with_structured_tag_is_accepted(self):
        metadata = {'version': '1.2.3', 'commit': 'test-source', 'built_at': '2026-10-17T00:00:00Z'}
        tag = 'metis_desktop_runtime_' + json.dumps(metadata, sort_keys=True, separators=(',', ':')).encode().hex()
        text = 'fixture: go1.26.4\n\tbuild\t-tags=' + tag + '\n\tbuild\t-trimpath=true\n'
        with patch.object(bundle, 'run', return_value=text):
            result = bundle.check_build_version(self.cli, '1.2.3', 'synthetic-go', 'test-source', '2026-10-17T00:00:00Z')
        self.assertEqual(result['Date'], metadata['built_at'])

    def test_runtime_identity_cannot_disagree_with_present_legacy_metadata(self):
        tag = bundle.runtime_build_tag('1.2.3', 'test-source', '2026-10-17T00:00:00Z')
        text = self.fake_run(['synthetic-go', 'version', '-m', self.cli]).replace('Commit=test-source', 'Commit=other-source')
        text += '\tbuild\t-tags=' + tag + '\n'
        with patch.object(bundle, 'run', return_value=text):
            with self.assertRaisesRegex(bundle.BundleError, 'conflicts'):
                bundle.check_build_version(self.cli, '1.2.3', 'synthetic-go', 'test-source')

    def test_conflicting_or_duplicate_runtime_identity_tags_fail(self):
        metadata = {'version': '1.2.3', 'commit': 'test-source', 'built_at': '2026-10-17T00:00:00Z'}
        tag = 'metis_desktop_runtime_' + json.dumps(metadata, sort_keys=True, separators=(',', ':')).encode().hex()
        for tags in [tag + ',' + tag, tag + ',metis_desktop_runtime_invalid']:
            with self.subTest(tags=tags):
                text = 'fixture: go1.26.4\n\tbuild\t-tags=' + tags + '\n\tbuild\t-trimpath=true\n'
                with patch.object(bundle, 'run', return_value=text):
                    with self.assertRaises(bundle.BundleError):
                        bundle.check_build_version(self.cli, '1.2.3', 'synthetic-go', 'test-source')

    def test_unversioned_cli_cannot_be_stamped_as_current(self):
        with patch.object(bundle, 'run', return_value='binary: go1.26\n'):
            with self.assertRaisesRegex(bundle.BundleError, 'Go build version'):
                bundle.prepare(self.args())

    def test_manifest_target_and_version_must_match(self):
        self.prepared()
        directory = self.app / 'Contents/Resources/bin'
        for field, value in [('version', '0.0.1'), ('goarch', 'amd64'), ('goos', 'linux'), ('size_bytes', True)]:
            with self.subTest(field=field):
                path = directory / 'metis-runtime.json'
                original = path.read_text()
                data = json.loads(original)
                data[field] = value
                path.write_text(json.dumps(data))
                with self.assertRaises(bundle.BundleError):
                    bundle.verify_payload(directory, 'darwin/arm64', '1.2.3', mac=True)
                path.write_text(original)

    def test_symlink_payload_and_parent_rejected(self):
        self.prepared()
        payload = self.app / 'Contents/Resources/bin/metis'
        payload.unlink()
        payload.symlink_to(self.cli)
        with self.assertRaisesRegex(bundle.BundleError, 'regular'):
            bundle.verify_payload(payload.parent, 'darwin/arm64', '1.2.3', mac=True)
        with self.assertRaisesRegex(bundle.BundleError, 'symbolic links'):
            bundle.safe_directory(payload / 'child')

    def test_elf_and_pe_headers_are_checked(self):
        elf = bytearray(64)
        elf[:6] = b'\x7fELF\x02\x01'
        struct.pack_into('<H', elf, 18, 183)
        self.cli.write_bytes(elf)
        self.assertEqual(bundle.binary_target(self.cli), ('linux', {'arm64'}))
        pe = bytearray(128)
        pe[:2] = b'MZ'
        struct.pack_into('<I', pe, 60, 80)
        pe[80:84] = b'PE\x00\x00'
        struct.pack_into('<H', pe, 84, 0x8664)
        self.cli.write_bytes(pe)
        self.assertEqual(bundle.binary_target(self.cli), ('windows', {'amd64'}))

    def test_universal_requires_both_supported_architectures(self):
        header = bytearray(48)
        struct.pack_into('>II', header, 0, 0xcafebabe, 2)
        struct.pack_into('>I', header, 8, 0x01000007)
        struct.pack_into('>I', header, 28, 0x0100000C)
        self.cli.write_bytes(header)
        bundle.check_target(self.cli, 'darwin/universal')
        struct.pack_into('>I', header, 28, 0x01000007)
        self.cli.write_bytes(header)
        with self.assertRaisesRegex(bundle.BundleError, 'exactly'):
            bundle.check_target(self.cli, 'darwin/universal')


class RealGoTrimpathBuildInfoTests(unittest.TestCase):
    """Compile only a tiny isolated fixture, never the Metis CLI/application."""
    @unittest.skipUnless(shutil.which('go'), 'Go compiler unavailable for tiny buildinfo fixture')
    def test_trimpath_marker_survives_actual_go_build_and_unmarked_input_is_rejected(self):
        compiler = os.environ.get('METIS_TEST_GO') or shutil.which('go')
        with tempfile.TemporaryDirectory(prefix='metis-buildinfo-test-') as temporary:
            root = Path(temporary)
            (root / 'go.mod').write_text('module github.com/Ricardo-M-L/metis\n\ngo 1.23\n')
            (root / 'internal/version').mkdir(parents=True)
            (root / 'internal/version/version.go').write_text(
                'package version\nvar Version="dev"\nvar Commit="unknown"\nvar Date="unknown"\n')
            (root / 'main.go').write_text(
                'package main\nimport ("fmt"; "github.com/Ricardo-M-L/metis/internal/version")\n'
                'func main(){fmt.Println(version.Version,version.Commit,version.Date)}\n')
            flags = ('-s -w -X ' + bundle.VERSION_PACKAGE + '.Version=1.2.3 -X '
                     + bundle.VERSION_PACKAGE + '.Commit=fixture-source -X '
                     + bundle.VERSION_PACKAGE + '.Date=2026-10-18T00:00:00Z')
            env = {**os.environ, 'GOWORK': 'off', 'CGO_ENABLED': '0', 'GOOS': 'linux', 'GOARCH': 'amd64'}
            tag = bundle.runtime_build_tag('1.2.3', 'fixture-source', '2026-10-18T00:00:00Z')
            for tagged in [True, False]:
                output = root / ('tagged' if tagged else 'unmarked')
                command = [compiler, 'build', '-mod=readonly', '-trimpath', '-ldflags', flags]
                if tagged:
                    command += ['-tags', tag]
                built = subprocess.run(command + ['-o', str(output), '.'], cwd=root, env=env,
                                       capture_output=True, text=True, timeout=90)
                self.assertEqual(built.returncode, 0, built.stderr)
                info = bundle.run([compiler, 'version', '-m', output])
                self.assertIn('-trimpath=true', info)
                self.assertNotIn('-ldflags=', info)
                bundle.check_target(output, 'linux/amd64')
                if tagged:
                    self.assertIn('-tags=' + tag, info)
                    metadata = bundle.check_build_version(output, '1.2.3', compiler,
                                                          'fixture-source', '2026-10-18T00:00:00Z')
                    self.assertEqual(metadata['Commit'], 'fixture-source')
                    with self.assertRaises(bundle.BundleError):
                        bundle.check_build_version(output, '1.2.3', compiler, 'wrong-source')
                else:
                    with self.assertRaisesRegex(bundle.BundleError, 'build version'):
                        bundle.check_build_version(output, '1.2.3', compiler, 'fixture-source')


class DesktopPackagingContractTests(unittest.TestCase):
    def setUp(self):
        self.repo = Path(__file__).resolve().parents[1]

    def test_signed_bundle_hash_is_checked_after_final_outer_signature(self):
        text = (self.repo / 'scripts/package-macos-signed.sh').read_text()
        bundler = text.index('python3 "$repo_root/scripts/prepare-desktop-cli.py"')
        sign = text.index('codesign --force --options runtime --timestamp --sign "$identity" "$app_path"')
        final_check = text.index('--app "$app_path" --verify-only --execute-version')
        self.assertLess(bundler, sign)
        self.assertLess(sign, final_check)
        self.assertNotIn('codesign --force --deep', text)
        self.assertIn('--sign-identity "$identity"', text)

    def test_ci_prepares_payload_before_production_and_checks_packaged_executables(self):
        text = (self.repo / '.github/workflows/release.yml').read_text().split('  desktop-build:', 1)[1]
        self.assertLess(text.index('Prepare same-source embedded CLI'), text.index('go build -tags'))
        self.assertIn('python scripts/prepare-desktop-cli.py --platform "$PLATFORM"', text)
        self.assertGreaterEqual(text.count('--runtime-check'), 3)
        self.assertIn('--verify-only --execute-version', text)
        self.assertNotIn('codesign --force --deep', text)

    def test_published_new_tags_require_cli_digest_version_and_architectures(self):
        text = (self.repo / '.github/workflows/release-published.yml').read_text()
        self.assertEqual(text.count('if [ -f source/scripts/prepare-desktop-cli.py ]; then'), 2)
        self.assertEqual(text.count('--app "$app" --verify-only --execute-version'), 2)
        self.assertEqual(text.count('lipo "$app/Contents/Resources/bin/metis" -verify_arch x86_64 arm64'), 2)

if __name__ == '__main__':
    unittest.main()
