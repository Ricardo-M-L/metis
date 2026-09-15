#!/usr/bin/env python3
"""Bundle the compiled computer-use catalog's archives, without running helpers.

The catalog embedded by internal/computeruse/manifest.go is the only release
authority. Local artifacts are merely a download substitute, never a manifest.
Run before signing: bundle-computer-use.py --app build/bin/METIS.app
"""

import argparse
import ctypes
import gzip
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import stat
import sys
import tarfile
import tempfile
import time
import urllib.parse
import urllib.request


CATALOG = Path(__file__).resolve().parent.parent / "internal/computeruse/releases.json"
PROTOCOL_VERSION = 1
TARGETS = ("darwin-arm64", "darwin-amd64")
MAX_BYTES = 128 << 20
MAX_CATALOG_BYTES = 1 << 20
DOWNLOAD_SECONDS = 120
SOCKET_SECONDS = 15
CHUNK = 64 << 10


class BundleError(ValueError):
    """A catalog, artifact, or bundle path failed validation."""


def checked_path(value, *, directory=False):
    """Reject symlinks in every existing path component, including the leaf."""
    path = Path(os.path.abspath(value))
    for parent in (*reversed(path.parents), path):
        info = parent.lstat()
        if stat.S_ISLNK(info.st_mode):
            raise BundleError(f"symlink is not allowed: {parent}")
        if parent != path or directory:
            if not stat.S_ISDIR(info.st_mode):
                raise BundleError(f"not a regular directory: {parent}")
        elif not stat.S_ISREG(info.st_mode):
            raise BundleError(f"not a regular file: {parent}")
    return path


def open_regular(path):
    path = checked_path(path)
    before = path.lstat()
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    after = os.fstat(fd)
    if not stat.S_ISREG(after.st_mode) or (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
        os.close(fd)
        raise BundleError(f"file changed while opening: {path}")
    return os.fdopen(fd, "rb")


def https_url(value):
    if not isinstance(value, str) or any(ord(c) <= 32 or ord(c) == 127 for c in value):
        raise BundleError("release URL must be HTTPS without whitespace or controls")
    try:
        parsed = urllib.parse.urlsplit(value)
    except ValueError as error:
        raise BundleError("release URL is malformed") from error
    if parsed.scheme != "https" or not parsed.hostname or parsed.username is not None or parsed.password is not None or parsed.fragment:
        raise BundleError("release URL must be HTTPS without credentials or fragment")
    try:
        parsed.port
    except ValueError as error:
        raise BundleError("release URL has an invalid port") from error
    if "\\" in parsed.netloc:
        raise BundleError("release URL has an invalid host")
    return parsed


def artifact_name(release):
    name = PurePosixPath(https_url(release.get("url")).path).name
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*\.tar\.gz", name):
        raise BundleError("release URL must have a safe .tar.gz basename")
    return name


def select_releases(catalog):
    if not isinstance(catalog, dict) or set(catalog) != {"releases"} or not isinstance(catalog["releases"], list):
        raise BundleError("catalog must contain a releases array")
    releases = catalog["releases"]
    if not releases:
        return []
    selected = {}
    for release in releases:
        if not isinstance(release, dict):
            raise BundleError("catalog release must be an object")
        target = release.get("target")
        if target not in TARGETS or type(release.get("protocolVersion")) is not int or release["protocolVersion"] != PROTOCOL_VERSION or target in selected:
            continue
        # Match Manager.release(): the first compatible entry for each target.
        if not isinstance(release.get("version"), str) or not release["version"].strip():
            raise BundleError("release requires a version")
        for key in ("sha256", "binarySha256"):
            if not isinstance(release.get(key), str) or not re.fullmatch(r"[0-9a-f]{64}", release[key]):
                raise BundleError(f"release requires a lowercase {key} digest")
        size = release.get("size", 0)
        if type(size) is not int or size < 0 or size > MAX_BYTES:
            raise BundleError("release size exceeds the artifact limit")
        if release.get("format") != "tar.gz":
            raise BundleError("Desktop computer-use releases must use tar.gz")
        binary = release.get("binaryPath")
        if not isinstance(binary, str) or "\\" in binary or ":" in binary or "\x00" in binary or binary.startswith("/") or any(part in ("", ".", "..") for part in binary.split("/")) or PurePosixPath(binary).name != "metis-cu":
            raise BundleError("release binaryPath must be a safe relative metis-cu path")
        artifact_name(release)
        selected[target] = release
    missing = set(TARGETS) - set(selected)
    if missing:
        raise BundleError("catalog lacks compatible releases for " + ", ".join(sorted(missing)))
    result = [selected[target] for target in TARGETS]
    if len({artifact_name(release) for release in result}) != len(TARGETS):
        raise BundleError("selected release archive basenames must be distinct")
    return result


def load_catalog():
    with open_regular(CATALOG) as source:
        data = source.read(MAX_CATALOG_BYTES + 1)
    if len(data) > MAX_CATALOG_BYTES:
        raise BundleError("catalog exceeds size limit")

    def unique_object(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise BundleError(f"duplicate catalog key: {key}")
            result[key] = value
        return result

    return select_releases(json.loads(data, object_pairs_hook=unique_object))


class SafeRedirects(urllib.request.HTTPRedirectHandler):
    max_redirections = 5
    max_repeats = 2

    def __init__(self, deadline):
        self.deadline = deadline
        self.count = 0

    def redirect_request(self, request, response, code, message, headers, new_url):
        https_url(new_url)
        self.count += 1
        if self.count > self.max_redirections or time.monotonic() >= self.deadline:
            raise BundleError("download redirect count or deadline exceeded")
        return super().redirect_request(request, response, code, message, headers, new_url)


def copy_verified(source, destination, release, deadline=None):
    digest = hashlib.sha256()
    count = 0
    with destination.open("xb") as output:
        os.fchmod(output.fileno(), 0o644)
        while True:
            if deadline is not None and time.monotonic() >= deadline:
                raise BundleError("artifact download deadline exceeded")
            # read1 performs at most one underlying read, so a slow stream cannot
            # indefinitely postpone the total deadline by dribbling bytes.
            read = getattr(source, "read1", source.read)
            chunk = read(min(CHUNK, MAX_BYTES - count + 1))
            if not chunk:
                break
            count += len(chunk)
            if count > MAX_BYTES:
                raise BundleError("artifact exceeds size limit")
            output.write(chunk)
            digest.update(chunk)
        output.flush()
        os.fsync(output.fileno())
    if release.get("size", 0) and count != release["size"]:
        raise BundleError("artifact size differs from pinned size")
    if digest.hexdigest() != release["sha256"]:
        raise BundleError("archive SHA256 differs from compiled catalog")


def obtain_archive(release, destination, artifact_dir):
    if artifact_dir is not None:
        with open_regular(artifact_dir / artifact_name(release)) as source:
            copy_verified(source, destination, release)
        return
    deadline = time.monotonic() + DOWNLOAD_SECONDS
    opener = urllib.request.build_opener(SafeRedirects(deadline))
    request = urllib.request.Request(release["url"], headers={"Accept-Encoding": "identity", "User-Agent": "METIS-computer-use-bundler"})
    with opener.open(request, timeout=SOCKET_SECONDS) as source:
        https_url(source.geturl())
        if source.status != 200:
            raise BundleError(f"artifact download returned HTTP {source.status}")
        if source.headers.get("Content-Encoding", "identity").lower() != "identity":
            raise BundleError("unexpected HTTP content encoding")
        length = source.headers.get("Content-Length")
        if length is not None and (not length.isdigit() or int(length) > MAX_BYTES):
            raise BundleError("artifact Content-Length exceeds size limit")
        copy_verified(source, destination, release, deadline)


def verify_archive(filename, release):
    # Bound decompression, validate gzip CRC/truncation, and never extract files.
    with open_regular(filename) as raw, gzip.GzipFile(fileobj=raw) as compressed, tempfile.SpooledTemporaryFile(max_size=1 << 20) as uncompressed:
        total = 0
        while True:
            chunk = compressed.read(min(CHUNK, MAX_BYTES + tarfile.RECORDSIZE - total + 1))
            if not chunk:
                break
            total += len(chunk)
            if total > MAX_BYTES + tarfile.RECORDSIZE:
                raise BundleError("archive exceeds decompressed size limit")
            uncompressed.write(chunk)
        uncompressed.seek(0)
        with tarfile.open(fileobj=uncompressed, mode="r:") as archive:
            member = archive.next()
            if member is None or member.type not in (tarfile.REGTYPE, tarfile.AREGTYPE) or member.offset != 0 or member.offset_data != tarfile.BLOCKSIZE or member.pax_headers or member.sparse is not None:
                raise BundleError("archive must contain one ordinary regular metis-cu member")
            if member.name != release["binaryPath"] or member.mode != 0o755:
                raise BundleError("archive member must match binaryPath with mode 0755")
            if not 0 < member.size <= MAX_BYTES:
                raise BundleError("archive binary size is invalid")
            digest = hashlib.sha256()
            with archive.extractfile(member) as binary:
                while chunk := binary.read(CHUNK):
                    digest.update(chunk)
            if digest.hexdigest() != release["binarySha256"]:
                raise BundleError("binary SHA256 differs from compiled binarySha256")
            if archive.next() is not None:
                raise BundleError("archive must contain exactly one member")
            payload_end = member.offset_data + ((member.size + 511) // 512) * 512
            uncompressed.seek(payload_end)
            tail = uncompressed.read()
            if len(tail) < 1024 or total % 512 or any(tail):
                raise BundleError("archive has invalid end markers or trailing data")


def publish_stage(stage, destination):
    """Atomic directory publication that never replaces even an empty directory."""
    library = ctypes.CDLL(None, use_errno=True)
    if sys.platform == "darwin":
        rename = library.renamex_np
        rename.argtypes = (ctypes.c_char_p, ctypes.c_char_p, ctypes.c_uint)
        result = rename(os.fsencode(stage), os.fsencode(destination), 0x4)  # RENAME_EXCL
    elif sys.platform.startswith("linux") and hasattr(library, "renameat2"):
        rename = library.renameat2
        rename.argtypes = (ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint)
        result = rename(-100, os.fsencode(stage), -100, os.fsencode(destination), 1)  # RENAME_NOREPLACE
    else:
        raise BundleError("atomic no-replace publication requires macOS or Linux renameat2")
    if result:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error), str(destination))


def bundle_app(app, artifact_dir=None, allow_empty=False):
    app = checked_path(app, directory=True)
    if app.suffix != ".app":
        raise BundleError("--app must be an existing .app bundle")
    resources = checked_path(app / "Contents/Resources", directory=True)
    destination = resources / "computer-use"
    if os.path.lexists(destination):
        raise BundleError("computer-use bundle output already exists; refusing to overwrite")
    releases = load_catalog()
    if not releases:
        if allow_empty:
            return []
        raise BundleError("compiled computer-use catalog is empty; production Desktop bundling requires reviewed releases (development candidates may explicitly use --allow-empty)")
    if artifact_dir is not None:
        artifact_dir = checked_path(artifact_dir, directory=True)
    stage = Path(tempfile.mkdtemp(prefix=".computer-use-stage-", dir=resources))
    try:
        for release in releases:
            archive = stage / artifact_name(release)
            obtain_archive(release, archive, artifact_dir)
            verify_archive(archive, release)
        os.chmod(stage, 0o755)
        publish_stage(stage, destination)
    finally:
        if stage.exists():
            shutil.rmtree(stage)
    return [destination / artifact_name(release) for release in releases]


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--app", required=True, type=Path)
    parser.add_argument("--artifact-dir", type=Path, help="offline archives named exactly as pinned URL basenames")
    parser.add_argument("--allow-empty", action="store_true", help="explicit development candidate only: skip an empty catalog")
    args = parser.parse_args(argv)
    try:
        archives = bundle_app(args.app, args.artifact_dir, args.allow_empty)
    except (BundleError, OSError, EOFError, tarfile.TarError, json.JSONDecodeError) as error:
        print(f"bundle-computer-use: {error}", file=sys.stderr)
        return 1
    if not archives:
        print("bundle-computer-use: SKIPPED empty catalog (--allow-empty development candidate)")
    else:
        print("bundle-computer-use: bundled verified archives for " + ", ".join(TARGETS))
    return 0


if __name__ == "__main__":
    sys.exit(main())
