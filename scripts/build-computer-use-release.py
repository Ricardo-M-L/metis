#!/usr/bin/env python3
"""Build deterministic macOS Computer Use archives and a pinned catalog.

The release workflow builds the native helper from a reviewed, pinned
``metis-cu`` commit. This script only packages already-built binaries; it
never downloads source or invents release metadata. Both target binaries must
be present before the catalog is written.
"""

import argparse
import gzip
import hashlib
import json
from pathlib import Path
import re
import os
import stat
import tarfile
import tempfile
import urllib.parse


TARGETS = ("darwin-arm64", "darwin-amd64")
PROTOCOL_VERSION = 1
MAX_BYTES = 128 << 20
VERSION_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._+~-]{0,255}\Z")


class ReleaseBuildError(ValueError):
    """A helper binary or release metadata failed validation."""


def digest(path):
    value = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1 << 20), b""):
            value.update(block)
    return value.hexdigest()


def checked_binary(path):
    path = Path(path).absolute()
    if not path.exists():
        raise ReleaseBuildError(f"helper binary does not exist: {path}")
    if path.is_symlink():
        raise ReleaseBuildError(f"helper binary must be a regular non-symlink file: {path}")
    info = path.stat()
    if not stat.S_ISREG(info.st_mode):
        raise ReleaseBuildError(f"helper binary must be a regular non-symlink file: {path}")
    if not info.st_mode & 0o111:
        raise ReleaseBuildError(f"helper binary is not executable: {path}")
    if not 0 < info.st_size <= MAX_BYTES:
        raise ReleaseBuildError(f"helper binary size is invalid: {path}")
    return path


def https_base(value):
    try:
        parsed = urllib.parse.urlsplit(value)
    except ValueError as error:
        raise ReleaseBuildError("base URL is malformed") from error
    if parsed.scheme != "https" or not parsed.netloc or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ReleaseBuildError("base URL must be HTTPS without credentials, query, or fragment")
    return value.rstrip("/")


def archive_name(version, target):
    """Return the stable published filename for a target.

    The helper version lives in the signed catalog. Keeping the asset name
    stable lets the release contract and Desktop bundle lookup remain
    independent of the upstream helper's version string.
    """
    del version
    return f"metis-cu-{target}.tar.gz"


def write_archive(binary, destination):
    """Write a reproducible USTAR+gzip archive containing exactly metis-cu."""
    destination.parent.mkdir(parents=True, exist_ok=True)
    with destination.open("wb") as raw:
        # A fixed gzip header and fixed tar metadata make the archive digest
        # reproducible for the same helper bytes.
        with gzip.GzipFile(fileobj=raw, mode="wb", filename="", mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as archive:
                member = tarfile.TarInfo("metis-cu")
                member.size = binary.stat().st_size
                member.mode = 0o755
                member.uid = 0
                member.gid = 0
                member.uname = ""
                member.gname = ""
                member.mtime = 0
                with binary.open("rb") as source:
                    archive.addfile(member, source)
    os.chmod(destination, 0o644)


def build_release(input_dir, output_dir, catalog, version, base_url):
    if not isinstance(version, str) or not VERSION_RE.fullmatch(version):
        raise ReleaseBuildError("version must be a safe non-empty release identifier")
    base_url = https_base(base_url)
    input_dir = Path(input_dir).resolve()
    output_dir = Path(output_dir).resolve()
    catalog = Path(catalog).resolve()
    if not input_dir.is_dir() or input_dir.is_symlink():
        raise ReleaseBuildError(f"input directory is not a regular directory: {input_dir}")
    output_dir.mkdir(parents=True, exist_ok=True)

    releases = []
    staged = []
    for target in TARGETS:
        binary = checked_binary(input_dir / f"metis-cu-{target}")
        name = archive_name(version, target)
        archive = output_dir / name
        write_archive(binary, archive)
        info = archive.stat()
        if not 0 < info.st_size <= MAX_BYTES:
            raise ReleaseBuildError(f"archive size is invalid: {archive}")
        binary_hash = digest(binary)
        archive_hash = digest(archive)
        releases.append({
            "version": version,
            "protocolVersion": PROTOCOL_VERSION,
            "target": target,
            "url": f"{base_url}/{name}",
            "sha256": archive_hash,
            "binarySha256": binary_hash,
            "format": "tar.gz",
            "binaryPath": "metis-cu",
            "size": info.st_size,
        })
        staged.append((archive, archive_hash))

    # Sidecars are published beside the archives and are useful for release
    # inventory checks. They are not used as runtime trust material.
    for archive, archive_hash in staged:
        sidecar = archive.with_name(archive.name + ".sha256")
        sidecar.write_text(f"{archive_hash}  {archive.name}\n", encoding="utf-8")
        os.chmod(sidecar, 0o644)

    catalog.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps({"releases": releases}, indent=2, ensure_ascii=False) + "\n"
    with tempfile.NamedTemporaryFile("w", encoding="utf-8", dir=catalog.parent, prefix=f".{catalog.name}.", delete=False) as temporary:
        temporary.write(payload)
        temporary.flush()
        os.fsync(temporary.fileno())
        temporary_name = Path(temporary.name)
    os.chmod(temporary_name, 0o644)
    os.replace(temporary_name, catalog)
    return releases


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input-dir", type=Path, required=True, help="directory containing metis-cu-darwin-{arm64,amd64}")
    parser.add_argument("--output-dir", type=Path, required=True, help="directory for archives and checksum sidecars")
    parser.add_argument("--catalog", type=Path, required=True, help="output internal/computeruse/releases.json")
    parser.add_argument("--version", required=True)
    parser.add_argument("--base-url", required=True, help="HTTPS GitHub release download base URL")
    args = parser.parse_args(argv)
    try:
        releases = build_release(args.input_dir, args.output_dir, args.catalog, args.version, args.base_url)
    except (OSError, ReleaseBuildError) as error:
        parser.error(str(error))
    print("computer-use release: " + ", ".join(release["target"] for release in releases))
    print(f"computer-use catalog: {args.catalog}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
