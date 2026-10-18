#!/usr/bin/env python3
"""Build and verify the CLI shipped inside Desktop, from the same checkout.

macOS: run after Wails, before signing the outer app. Other targets: run before
Wails so production go:embed includes the target's private runtime payload.
This script never downloads a CLI or reads signing credentials.
"""
from __future__ import annotations
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import plistlib
import re
import shlex
import shutil
import struct
import subprocess
import sys
import tempfile

VERSION_PACKAGE = 'github.com/Ricardo-M-L/metis/internal/version'
MAX_BINARY = 256 * 1024 * 1024
BUILD_TAG_PREFIX = 'metis_desktop_runtime_'
TARGETS = {'darwin/arm64', 'darwin/amd64', 'darwin/universal',
           'linux/amd64', 'linux/arm64', 'windows/amd64', 'windows/arm64'}


class BundleError(ValueError):
    pass


def run(argv, *, cwd=None, env=None):
    result = subprocess.run([str(value) for value in argv], cwd=cwd, env=env,
                            capture_output=True, text=True, timeout=600)
    if result.returncode:
        raise BundleError(f'{Path(str(argv[0])).name} failed ({result.returncode}): {result.stderr.strip()}')
    return result.stdout


def release_version(repo: Path) -> str:
    version = (repo / 'VERSION').read_text().strip()
    if not re.fullmatch(r'(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?', version):
        raise BundleError('VERSION is invalid')
    metadata = json.loads((repo / 'metis-desktop/wails.json').read_text())
    if metadata.get('info', {}).get('productVersion') != version:
        raise BundleError('Wails productVersion differs from VERSION')
    return version


def binary_target(path: Path) -> tuple[str, set[str]]:
    """Inspect executable headers without executing an input artifact."""
    if path.is_symlink() or not path.is_file() or not 0 < path.stat().st_size <= MAX_BINARY:
        raise BundleError('CLI must be a regular, bounded file')
    with path.open('rb') as stream:
        header = stream.read(4096)
    cpu_names = {0x01000007: 'amd64', 0x0100000C: 'arm64'}
    if header[:4] in (b'\xcf\xfa\xed\xfe', b'\xfe\xed\xfa\xcf'):
        endian = '<' if header[0] == 0xcf else '>'
        cpu = struct.unpack_from(endian + 'I', header, 4)[0]
        if cpu not in cpu_names:
            raise BundleError('unsupported Mach-O architecture')
        return 'darwin', {cpu_names[cpu]}
    if header[:4] in (b'\xca\xfe\xba\xbe', b'\xca\xfe\xba\xbf'):
        count = struct.unpack_from('>I', header, 4)[0]
        stride = 32 if header[3] == 0xbf else 20
        if count != 2 or len(header) < 8 + count * stride:
            raise BundleError('invalid universal Mach-O architecture table')
        cpus = [struct.unpack_from('>I', header, 8 + i * stride)[0] for i in range(count)]
        if set(cpus) != set(cpu_names):
            raise BundleError('universal CLI must contain exactly amd64 and arm64')
        return 'darwin', {cpu_names[cpu] for cpu in cpus}
    if header[:4] == b'\x7fELF' and len(header) >= 20:
        endian = '<' if header[5] == 1 else '>'
        machine = struct.unpack_from(endian + 'H', header, 18)[0]
        names = {62: 'amd64', 183: 'arm64'}
        if header[4] != 2 or machine not in names:
            raise BundleError('unsupported ELF architecture')
        return 'linux', {names[machine]}
    if header[:2] == b'MZ' and len(header) >= 64:
        offset = struct.unpack_from('<I', header, 60)[0]
        with path.open('rb') as stream:
            stream.seek(offset)
            pe = stream.read(6)
        names = {0x8664: 'amd64', 0xAA64: 'arm64'}
        if len(pe) != 6 or pe[:4] != b'PE\x00\x00' or struct.unpack_from('<H', pe, 4)[0] not in names:
            raise BundleError('unsupported PE architecture')
        return 'windows', {names[struct.unpack_from('<H', pe, 4)[0]]}
    raise BundleError('CLI has no supported native executable header')


def check_target(path: Path, platform: str):
    goos, arch = platform.split('/')
    expected = {'amd64', 'arm64'} if arch == 'universal' else {arch}
    if binary_target(path) != (goos, expected):
        raise BundleError(f'CLI executable architecture differs from {platform}')


def runtime_build_tag(version: str, commit: str, built_at: str) -> str:
    """Record public build identity in Go's structured, trimpath-safe settings."""
    value = {'version': version, 'commit': commit, 'built_at': built_at}
    if (not all(isinstance(part, str) and part for part in value.values())
            or not re.fullmatch(r'[A-Za-z0-9_.+-]{1,160}', commit)
            or not re.fullmatch(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ', built_at)):
        raise BundleError('runtime build tag needs explicit valid version, commit and UTC date')
    return BUILD_TAG_PREFIX + json.dumps(value, sort_keys=True, separators=(',', ':')).encode().hex()


def check_build_version(path: Path, version: str, go: str, commit=None, built_at=None):
    text = run([go, 'version', '-m', path])
    assignments, markers = [], []
    for line in text.splitlines():
        setting = re.fullmatch(r'\s*build\s+(-[a-z]+)=(.*)', line)
        if setting is None:
            continue
        key, value = setting.groups()
        if key == '-tags':
            tags = shlex.split(value)
            for tag in re.split(r'[,\s]+', ' '.join(tags)):
                if tag.startswith(BUILD_TAG_PREFIX):
                    markers.append(tag)
        elif key == '-ldflags':
            # Go deliberately omits -ldflags under -trimpath (issue 52372).
            # Retain compatibility with verifiable older non-trimpath builds.
            flags = shlex.split(value)
            if len(flags) == 1:
                flags = shlex.split(flags[0])
            for index, token in enumerate(flags):
                if token == '-X' and index + 1 < len(flags):
                    assignments.append(flags[index + 1])
                elif token.startswith('-X='):
                    assignments.append(token[3:])
    if len(markers) > 1:
        raise BundleError('CLI has multiple runtime identity tags')
    tagged = None
    if markers:
        try:
            encoded = markers[0][len(BUILD_TAG_PREFIX):]
            if len(encoded) > 2048:
                raise ValueError('oversized tag')
            value = json.loads(bytes.fromhex(encoded))
            if (set(value) != {'version', 'commit', 'built_at'}
                    or runtime_build_tag(value['version'], value['commit'], value['built_at']) != markers[0]):
                raise ValueError('noncanonical runtime identity')
            tagged = {'Version': value['version'], 'Commit': value['commit'], 'Date': value['built_at']}
        except (ValueError, TypeError, KeyError) as error:
            raise BundleError('CLI has an invalid runtime identity tag') from error
    legacy = {}
    for field in ['Version', 'Commit', 'Date']:
        values = [value.split('=', 1)[1] for value in assignments
                  if value.startswith(VERSION_PACKAGE + '.' + field + '=')]
        if len(values) > 1:
            raise BundleError(f'CLI has multiple Go build {field} assignments')
        if values:
            legacy[field] = values[0]
    if tagged and any(tagged.get(field) != value for field, value in legacy.items()):
        raise BundleError('CLI runtime identity conflicts with legacy linker metadata')
    metadata = tagged or legacy
    if metadata.get('Version') != version:
        raise BundleError('CLI Go build version must explicitly match VERSION')
    for field, expected in [('Commit', commit), ('Date', built_at)]:
        if expected is not None and metadata.get(field) != expected:
            raise BundleError(f'CLI Go build {field} differs from requested source metadata')
    return metadata


def check_app_version(app: Path, version: str):
    plist = app / 'Contents/Info.plist'
    content = plist.read_bytes()
    info = plistlib.loads(content, fmt=plistlib.FMT_BINARY if content.startswith(b'bplist00') else plistlib.FMT_XML)
    if info.get('CFBundleShortVersionString') != version:
        raise BundleError('app CFBundleShortVersionString differs from VERSION')


def safe_directory(path: Path):
    for parent in [path, *path.parents]:
        if parent.is_symlink():
            raise BundleError('runtime destination may not contain symbolic links')
    path.mkdir(parents=True, exist_ok=True)


def verify_payload(directory: Path, platform: str, version: str, *, mac=False) -> dict:
    manifest_path = directory / ('metis-runtime.json' if mac else 'manifest.json')
    if manifest_path.is_symlink() or not manifest_path.is_file():
        raise BundleError('runtime manifest is missing or a symbolic link')
    manifest = json.loads(manifest_path.read_text())
    goos, goarch = platform.split('/')
    binary = 'metis.exe' if goos == 'windows' else 'metis'
    if (manifest.get('schema_version') != 1 or manifest.get('version') != version
            or manifest.get('goos') != goos or manifest.get('goarch') != goarch
            or manifest.get('binary', binary) != binary):
        raise BundleError('runtime manifest schema, platform or version differs')
    payload = directory / binary
    check_target(payload, platform)
    size = payload.stat().st_size
    digest = hashlib.sha256(payload.read_bytes()).hexdigest()
    if (type(manifest.get('size_bytes')) is not int or manifest['size_bytes'] != size
            or manifest.get('sha256') != digest):
        raise BundleError('runtime payload hash or size differs from manifest')
    if goos != 'windows' and not os.access(payload, os.X_OK):
        raise BundleError('runtime payload is not executable')
    return manifest


def prepare(args) -> dict:
    repo = args.repo.resolve(strict=True)
    version = release_version(repo)
    platform = args.platform
    if platform not in TARGETS:
        raise BundleError('unsupported target platform')
    goos, arch = platform.split('/')
    mac = goos == 'darwin'
    if mac:
        if args.app is None:
            raise BundleError('darwin payload requires --app after Wails build')
        if args.app.is_symlink():
            raise BundleError('app must not be a symbolic link')
        app = args.app.resolve(strict=True)
        check_app_version(app, version)
        directory = app / 'Contents/Resources/bin'
    else:
        if args.app is not None:
            raise BundleError('--app is only supported for darwin')
        directory = args.output or repo / 'metis-desktop/embedded-runtime' / f'{goos}-{arch}'
    if args.verify_only:
        manifest = verify_payload(directory, platform, version, mac=mac)
        if args.execute_version:
            text = run([directory / manifest.get('binary', 'metis.exe' if goos == 'windows' else 'metis'), 'version'])
            if not text.split() or text.split()[0].removeprefix('v') != version:
                raise BundleError('bundled CLI reports a different runtime version')
        return manifest
    safe_directory(directory)
    commit = args.commit or run(['git', 'rev-parse', 'HEAD'], cwd=repo).strip()
    built_at = args.date or (None if args.cli else dt.datetime.now(dt.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'))
    if not re.fullmatch(r'[A-Za-z0-9_.+-]{1,160}', commit):
        raise BundleError('invalid build commit')
    if built_at is not None and not re.fullmatch(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ', built_at):
        raise BundleError('invalid UTC build date')
    if args.source_manifest_sha256 and not re.fullmatch('[0-9a-f]{64}', args.source_manifest_sha256):
        raise BundleError('invalid source manifest SHA-256')
    binary_name = 'metis.exe' if goos == 'windows' else 'metis'
    # Build/sign in a temporary sibling, then replace the payload and its hash.
    with tempfile.TemporaryDirectory(prefix='.runtime-build-', dir=directory.parent) as temporary:
        temporary = Path(temporary)
        output = temporary / binary_name
        arches = ['arm64', 'amd64'] if arch == 'universal' else [arch]
        if args.cli:
            check_target(args.cli, platform)
            shutil.copyfile(args.cli, output)
            recorded_dates = []
            if arch == 'universal':
                for target_arch in arches:
                    thin = temporary / target_arch
                    run(['lipo', output, '-thin', 'x86_64' if target_arch == 'amd64' else target_arch, '-output', thin])
                    metadata = check_build_version(thin, version, args.go, commit, built_at)
                    recorded_dates.append(metadata.get('Date'))
            else:
                metadata = check_build_version(output, version, args.go, commit, built_at)
                recorded_dates.append(metadata.get('Date'))
            # A reused executable retains its actual build time. Missing or
            # conflicting metadata cannot be relabeled with packaging time.
            if built_at is None:
                if (any(not isinstance(value, str) or not re.fullmatch(
                        r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ', value) for value in recorded_dates)
                        or len(set(recorded_dates)) != 1):
                    raise BundleError('CLI Go build Date must be valid and identical across reused slices')
                built_at = recorded_dates[0]
        else:
            flags = (f'-s -w -X {VERSION_PACKAGE}.Version={version} '
                     f'-X {VERSION_PACKAGE}.Commit={commit} -X {VERSION_PACKAGE}.Date={built_at}')
            outputs = []
            for target_arch in arches:
                target = temporary / target_arch
                env = {**os.environ, 'CGO_ENABLED': '0', 'GOOS': goos, 'GOARCH': target_arch, 'GOWORK': 'off'}
                run([args.go, 'build', '-mod=readonly', '-trimpath',
                     '-tags', runtime_build_tag(version, commit, built_at), '-ldflags', flags,
                     '-o', target, './cmd/metis'], cwd=repo, env=env)
                check_target(target, f'{goos}/{target_arch}')
                check_build_version(target, version, args.go, commit, built_at)
                outputs.append(target)
            if arch == 'universal':
                run(['lipo', '-create', *outputs, '-output', output])
            else:
                shutil.copyfile(outputs[0], output)
        output.chmod(0o755)
        check_target(output, platform)
        if mac:
            sign = ['codesign', '--force', '--options', 'runtime']
            if args.sign_identity != '-':
                sign.append('--timestamp')
            run([*sign, '--sign', args.sign_identity, output])
            run(['codesign', '--verify', '--strict', output])
        manifest = {'schema_version': 1, 'version': version, 'goos': goos, 'goarch': arch,
                    'binary': binary_name, 'sha256': hashlib.sha256(output.read_bytes()).hexdigest(),
                    'size_bytes': output.stat().st_size, 'commit': commit, 'built_at': built_at}
        if args.source_manifest_sha256:
            manifest['source_manifest_sha256'] = args.source_manifest_sha256
        metadata = temporary / 'manifest.json'
        metadata.write_text(json.dumps(manifest, indent=2) + '\n')
        metadata.chmod(0o644)
        os.replace(output, directory / binary_name)
        os.replace(metadata, directory / ('metis-runtime.json' if mac else 'manifest.json'))
    return verify_payload(directory, platform, version, mac=mac)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--repo', type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument('--platform')
    parser.add_argument('--print-build-tag', action='store_true', help='print public trimpath-safe identity tag for a separate fresh CLI build; requires --commit and --date')
    parser.add_argument('--app', type=Path)
    parser.add_argument('--output', type=Path)
    parser.add_argument('--cli', type=Path, help='reuse a fresh same-source CLI with verifiable runtime identity (or legacy linker metadata)')
    parser.add_argument('--go', default=shutil.which('go') or 'go')
    parser.add_argument('--sign-identity', default='-', help='macOS CLI identity; default ad-hoc')
    parser.add_argument('--commit')
    parser.add_argument('--date')
    parser.add_argument('--source-manifest-sha256')
    parser.add_argument('--verify-only', action='store_true')
    parser.add_argument('--execute-version', action='store_true', help='also run verified host-compatible CLI version')
    args = parser.parse_args()
    try:
        if args.print_build_tag:
            print(runtime_build_tag(release_version(args.repo.resolve(strict=True)), args.commit, args.date))
        else:
            print(json.dumps(prepare(args), sort_keys=True))
        return 0
    except (BundleError, OSError, json.JSONDecodeError, subprocess.TimeoutExpired) as error:
        print(f'prepare-desktop-cli: {error}', file=sys.stderr)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
