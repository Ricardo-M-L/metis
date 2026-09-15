#!/usr/bin/env python3
"""Validate the managed METIS Computer Use helper contract.

The release builder can verify hashes and archive layout, but that is not
enough to prove that the executable can be managed by METIS.  This gate runs
the native helper's side-effect-free metadata command before an archive is
created.  A plain MCP server without the METIS descriptor/lifecycle contract
must therefore fail closed instead of becoming an official component.
"""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile


PROTOCOL_VERSION = 1
MAX_OUTPUT_BYTES = 64 << 10
VERSION_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._+~-]{0,255}\Z")
REQUIRED_CAPABILITIES = {
    "status",
    "stop",
    "end-turn",
    "serialized-input",
    "input-ownership",
}


class HelperValidationError(ValueError):
    """The helper does not satisfy the managed METIS contract."""


def checked_executable(path):
    path = Path(path).absolute()
    if not path.exists():
        raise HelperValidationError(f"helper executable does not exist: {path}")
    if path.is_symlink() or not path.is_file():
        raise HelperValidationError(f"helper must be a regular non-symlink file: {path}")
    if os.name != "nt" and not path.stat().st_mode & 0o111:
        raise HelperValidationError(f"helper is not executable: {path}")
    return path


def run_helper(path, *args, timeout=10):
    """Run a metadata-only command with a minimal, deterministic environment."""
    with tempfile.TemporaryDirectory(prefix="metis-cu-probe-") as cwd:
        env = {
            "PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
            "HOME": cwd,
            "PWD": cwd,
            "LANG": "C",
            "LC_ALL": "C",
            "TZ": "UTC",
        }
        try:
            completed = subprocess.run(
                [str(path), *args],
                cwd=cwd,
                env=env,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                timeout=timeout,
                check=False,
            )
        except subprocess.TimeoutExpired as error:
            raise HelperValidationError(f"helper metadata command timed out: {' '.join(args)}") from error
        if len(completed.stdout) > MAX_OUTPUT_BYTES or len(completed.stderr) > MAX_OUTPUT_BYTES:
            raise HelperValidationError("helper metadata command exceeded output limit")
        if completed.returncode != 0:
            detail = completed.stderr.decode("utf-8", "replace").strip()
            suffix = f": {detail}" if detail else ""
            raise HelperValidationError(
                f"helper metadata command failed ({completed.returncode}){suffix}"
            )
        return completed.stdout


def decode_one_json(raw):
    try:
        text = raw.decode("utf-8")
        decoder = json.JSONDecoder()
        value, end = decoder.raw_decode(text)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise HelperValidationError(f"helper returned invalid metadata JSON: {error}") from error
    if text[end:].strip():
        raise HelperValidationError("helper metadata contains trailing data")
    if not isinstance(value, dict):
        raise HelperValidationError("helper metadata must be a JSON object")
    return value


def validate(path, target, expected_version=None):
    path = checked_executable(path)
    descriptor = decode_one_json(run_helper(path, "--describe", "--json"))
    if descriptor.get("name") != "metis-cu":
        raise HelperValidationError(f"unexpected helper name: {descriptor.get('name')!r}")
    if descriptor.get("protocolVersion") != PROTOCOL_VERSION:
        raise HelperValidationError(
            f"incompatible helper protocol {descriptor.get('protocolVersion')!r}; "
            f"expected {PROTOCOL_VERSION}"
        )
    actual_target = f"{descriptor.get('platform')}-{descriptor.get('arch')}"
    if actual_target != target:
        raise HelperValidationError(f"helper target {actual_target!r} does not match {target!r}")
    version = descriptor.get("version")
    if not isinstance(version, str) or not VERSION_RE.fullmatch(version):
        raise HelperValidationError("helper reported an invalid version")
    if expected_version is not None and version != expected_version:
        raise HelperValidationError(
            f"helper descriptor version {version!r} does not match {expected_version!r}"
        )
    capabilities = descriptor.get("capabilities")
    if not isinstance(capabilities, list):
        raise HelperValidationError("helper capabilities must be a JSON array")
    missing = sorted(REQUIRED_CAPABILITIES - {item for item in capabilities if isinstance(item, str)})
    if missing:
        raise HelperValidationError("helper is missing required capabilities: " + ", ".join(missing))

    version_line = run_helper(path, "--version").decode("utf-8", "replace").strip().splitlines()
    if version_line != [f"metis-cu {version}"]:
        raise HelperValidationError(f"helper --version output is not stable: {version_line!r}")
    return descriptor


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--target", required=True, help="expected GOOS-GOARCH target")
    parser.add_argument("--version")
    args = parser.parse_args(argv)
    try:
        descriptor = validate(args.binary, args.target, args.version)
    except (OSError, HelperValidationError) as error:
        parser.error(str(error))
    print(
        "computer-use helper compatible: "
        f"{descriptor['version']} {descriptor['platform']}-{descriptor['arch']}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
