#!/usr/bin/env python3
"""Build a new macOS ARM64 local package without installing or publishing it.

The CLI and native application are built in a temporary source mirror. The
frontend is built independently before Wails uses -s. Wails also skips bindings
and module synchronization; its runtime wrapper writes affect only the mirror.
Reports are supporting evidence from the caller; this script does not treat
their presence as a passing query test.
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
import shutil
import signal
import subprocess
import sys
import tempfile
import time


VERSION_PACKAGE = "github.com/Ricardo-M-L/metis/internal/version"
SECRET_FIELDS = {
    "apikey", "accesstoken", "refreshtoken", "password", "clientsecret",
    "privatekey", "authorization", "bearertoken", "leasetoken", "eventtoken",
    "token", "secret",
}
EXCLUDED_REPORT_PARTS = {
    ".credentials", "credentials", ".private", "secrets", "logs", "sessions",
    "inbox", "outbox", "project-coordinator", "node_modules", "__pycache__",
}
EXCLUDED_REPORT_NAMES = {"auth.json", "config.json", "active-run.json", "progress.json"}


class PackageError(RuntimeError):
    pass


class Commands:
    """Every child has its own process group so interrupted builds are stopped."""

    def __init__(self) -> None:
        self.active: subprocess.Popen[bytes] | None = None
        self.steps: list[dict[str, object]] = []

    def stop(self) -> None:
        child = self.active
        if child is None:
            return
        try:
            os.killpg(child.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            child.wait(timeout=3)
        except subprocess.TimeoutExpired:
            pass
        # The parent command may already have exited while descendants remain.
        try:
            os.killpg(child.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        child.wait()
        self.active = None

    def run(self, name: str, args: list[str], cwd: Path,
            env: dict[str, str] | None = None, timeout: int = 1800,
            record: bool = True) -> bytes:
        if record:
            print(name, file=sys.stderr, flush=True)
        started = time.monotonic()
        self.active = subprocess.Popen(
            args, cwd=cwd, env=env, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, start_new_session=True,
        )
        try:
            stdout, _stderr = self.active.communicate(timeout=timeout)
            code = self.active.returncode
            if code:
                self.stop()
                # Do not echo arbitrary build output: it may contain credentials
                # from the caller's npm/Go environment.
                raise PackageError(f"{name} failed with exit code {code}")
            self.active = None
        except BaseException:
            self.stop()
            raise
        if record:
            self.steps.append({"step": name, "exit_code": 0,
                               "duration_seconds": round(time.monotonic() - started, 3)})
        return stdout


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def source_state(commands: Commands, repo: Path, excluded: list[Path]) -> dict[str, object]:
    pathspec = ["."]
    for path in excluded:
        try:
            relative = path.relative_to(repo).as_posix()
        except ValueError:
            continue
        pathspec.append(f":(top,exclude){relative}")
    def git(*args: str) -> bytes:
        return commands.run("read Git source state", ["git", *args, "--", *pathspec],
                            repo, timeout=60, record=False)
    commit = commands.run("read Git commit", ["git", "rev-parse", "HEAD"], repo,
                          timeout=60, record=False).decode().strip()
    status = git("status", "--porcelain=v1", "-z", "--untracked-files=all")
    diff = git("diff", "--binary", "HEAD")
    untracked: list[dict[str, object]] = []
    for encoded in sorted(git("ls-files", "--others", "--exclude-standard", "-z").split(b"\0")):
        if not encoded:
            continue
        relative = os.fsdecode(encoded)
        path = repo / relative
        if path.is_symlink():
            digest = hashlib.sha256(os.fsencode(os.readlink(path))).hexdigest()
            kind = "symlink"
        elif path.is_file():
            digest, kind = sha256_file(path), "file"
        else:
            raise PackageError("untracked source changed during fingerprinting")
        untracked.append({"path": relative, "sha256": digest, "kind": kind})
    identity: dict[str, object] = {
        "commit": commit,
        "dirty": bool(status),
        "uncommitted_diff_sha256": hashlib.sha256(diff).hexdigest(),
        "status_sha256": hashlib.sha256(status).hexdigest(),
        "untracked_files": untracked,
    }
    payload = json.dumps(identity, sort_keys=True, separators=(",", ":")).encode()
    identity["source_fingerprint_sha256"] = hashlib.sha256(payload).hexdigest()
    return identity


def copy_build_source(commands: Commands, repo: Path, mirror: Path,
                      excluded: list[Path]) -> dict[str, object]:
    """Copy current tracked/untracked source bytes, never an old HEAD checkout.

    Root build outputs are excluded. Tracked metis-desktop/build templates and
    icons are retained because they are inputs required for native packaging.
    No source symlink is permitted to resolve outside the temporary mirror.
    """
    generated = [Path(name) for name in (
        "dist", "build", "bin", "metis-desktop/build/bin",
        "metis-desktop/release-dist-local", "metis-desktop/frontend/dist",
        "metis-desktop/frontend/node_modules",
    )]
    pathspec = ["."] + [f":(top,exclude){path.as_posix()}" for path in generated]
    for path in excluded:
        if path.is_relative_to(repo):
            pathspec.append(f":(top,exclude){path.relative_to(repo).as_posix()}")
    paths = commands.run("read build source paths", ["git", "ls-files", "--cached", "--others",
                         "--exclude-standard", "-z", "--", *pathspec], repo,
                         timeout=60, record=False)
    mirror.mkdir(parents=True)
    # macOS tempfile paths may use /var while resolve() returns /private/var.
    # Compare destination paths against the same canonical directory identity.
    mirror = mirror.resolve(strict=True)
    copied = 0
    for encoded in sorted(set(paths.split(b"\0"))):
        if not encoded:
            continue
        relative = Path(os.fsdecode(encoded))
        if (relative.is_absolute() or ".." in relative.parts
                or any(part in {".git", ".credentials", ".private", "node_modules", "__pycache__"}
                       for part in relative.parts)):
            continue
        source, destination = repo / relative, mirror / relative
        if not source.exists() and not source.is_symlink():
            # A tracked working-tree deletion is part of the reviewed diff.
            continue
        if not source.resolve().is_relative_to(repo):
            raise PackageError("build source symlink leaves the checkout; an isolated copy is required")
        if not destination.parent.resolve().is_relative_to(mirror):
            raise PackageError("source destination leaves the temporary mirror")
        destination.parent.mkdir(parents=True, exist_ok=True)
        if source.is_symlink():
            target = os.readlink(source)
            if os.path.isabs(target):
                target = str(mirror / source.resolve().relative_to(repo))
            destination.symlink_to(target, target_is_directory=source.is_dir())
        elif source.is_file():
            shutil.copy2(source, destination)
        else:
            raise PackageError("build source path is not a regular file or symlink")
        copied += 1

    source_modules = repo / "metis-desktop/frontend/node_modules"
    if not source_modules.is_dir():
        raise PackageError("native frontend dependencies must already be installed")
    resolved_modules = source_modules.resolve()
    module_links: list[tuple[Path, Path]] = []
    for directory, dirs, files in os.walk(source_modules, followlinks=False):
        for name in [*dirs, *files]:
            path = Path(directory) / name
            if path.is_symlink():
                resolved = path.resolve()
                if not resolved.is_relative_to(resolved_modules):
                    raise PackageError("frontend dependency symlink leaves node_modules; materialize dependencies before packaging")
                if os.path.isabs(os.readlink(path)):
                    module_links.append((path.relative_to(source_modules), resolved.relative_to(resolved_modules)))
    mirror_modules = mirror / "metis-desktop/frontend/node_modules"
    # A symlink to the original node_modules would allow Vite cache writes in
    # the user's checkout. Copy the dependencies and keep only internal links.
    shutil.copytree(source_modules, mirror_modules, symlinks=True)
    for relative, target in module_links:
        link = mirror_modules / relative
        link.unlink()
        link.symlink_to(mirror_modules / target, target_is_directory=(resolved_modules / target).is_dir())
    return {"copied_source_files": copied, "frontend_dependencies_copied": True,
            "build_location": "temporary source mirror", "build_commands_in_original_checkout": False}


def redact_text(value: str) -> str:
    value = re.sub(r"-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----",
                   "[REDACTED PRIVATE KEY]", value, flags=re.S)
    value = re.sub(r"(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+", "Bearer [REDACTED]", value)
    value = re.sub(
        r"(?i)(\b(?:api[_ -]?key|access[_ -]?token|refresh[_ -]?token|"
        r"lease[_ -]?token|event[_ -]?token|password|client[_ -]?secret)\s*[:=]\s*)"
        r"[^\s,;]+", r"\1[REDACTED]", value,
    )
    return re.sub(r"\bsk-[A-Za-z0-9_-]{16,}\b", "[REDACTED API KEY]", value)


def redact_report(value: object) -> object:
    if isinstance(value, dict):
        result: dict[str, object] = {}
        for key, item in value.items():
            normalized = re.sub(r"[^a-z]", "", key.lower())
            result[key] = ("[REDACTED]" if normalized in SECRET_FIELDS and isinstance(item, str)
                           else redact_report(item))
        return result
    if isinstance(value, list):
        return [redact_report(item) for item in value]
    if isinstance(value, str):
        return redact_text(value)
    return value


def copy_reports(inputs: list[Path], stage: Path) -> list[dict[str, object]]:
    copied: list[dict[str, object]] = []
    for index, directory in enumerate(inputs, 1):
        for source in sorted(directory.rglob("*")):
            relative = source.relative_to(directory)
            if (source.suffix.lower() not in {".json", ".md"} or not source.is_file()
                    or source.is_symlink() or source.name.lower() in EXCLUDED_REPORT_NAMES
                    or any(part.lower() in EXCLUDED_REPORT_PARTS for part in relative.parts)
                    or any((directory / Path(*relative.parts[:n])).is_symlink()
                           for n in range(1, len(relative.parts)))):
                continue
            destination = stage / "reports" / f"{index:02d}-{directory.name}" / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            content = source.read_text(encoding="utf-8")
            if source.suffix.lower() == ".json":
                content = json.dumps(redact_report(json.loads(content)), ensure_ascii=False, indent=2) + "\n"
            else:
                content = redact_text(content)
            destination.write_text(content, encoding="utf-8")
            copied.append({"path": destination.relative_to(stage).as_posix(),
                           "sha256": sha256_file(destination), "supporting_evidence_only": True})
    return copied


def app_executable(app: Path) -> Path:
    content = (app / "Contents/Info.plist").read_bytes()
    # Wails emits XML beginning with a DOCTYPE, without an XML declaration.
    # Explicit format selection also preserves support for binary plists.
    fmt = plistlib.FMT_BINARY if content.startswith(b"bplist00") else plistlib.FMT_XML
    name = plistlib.loads(content, fmt=fmt).get("CFBundleExecutable")
    if not isinstance(name, str) or Path(name).name != name:
        raise PackageError("application has an invalid CFBundleExecutable")
    executable = app / "Contents/MacOS" / name
    if not executable.is_file() or not executable.resolve().is_relative_to(app.resolve()):
        raise PackageError("application executable is missing or leaves the bundle")
    return executable


def app_symlinks(app: Path) -> dict[str, str]:
    result: dict[str, str] = {}
    for directory, dirs, files in os.walk(app, followlinks=False):
        for name in [*dirs, *files]:
            path = Path(directory) / name
            if path.is_symlink():
                result[path.relative_to(app).as_posix()] = os.readlink(path)
    return result


def write_json(path: Path, value: object) -> None:
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def checked_release_version(repo: Path) -> tuple[str, dict[str, str]]:
    """Read and validate the current version; never modify release metadata."""
    version = (repo / "VERSION").read_text().strip()
    numeric = r"(?:0|[1-9][0-9]*)"
    identifier = r"[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*"
    match = re.fullmatch(rf"{numeric}\.{numeric}\.{numeric}(?:-({identifier}))?(?:\+({identifier}))?", version)
    if match is None or any(len(part) > 1 and part.startswith("0") and part.isdigit()
                            for part in (match.group(1) or "").split(".")):
        raise PackageError("VERSION must contain a valid semantic version; this script never bumps it")
    source_version = (repo / "internal/version/version.go").read_text()
    source_match = re.search(r'(?m)^\s*Version\s*=\s*"([^"]+)"', source_version)
    if source_match is None or source_match.group(1) != version:
        raise PackageError(f"internal/version.Version must match current VERSION {version}; metadata is not rewritten")
    checked = {"VERSION": version, "internal/version/version.go": version}
    for relative, field in (("metis-desktop/wails.json", "productVersion"),
                            ("install/npm/package.json", "version")):
        path = repo / relative
        if not path.exists():
            continue
        document = json.loads(path.read_text())
        current = (document.get("info", {}).get(field) if field == "productVersion"
                   else document.get(field))
        if current is not None:
            if current != version:
                raise PackageError(f"{relative} {field} must match current VERSION {version}; metadata is not rewritten")
            checked[relative] = current
    # The private Vite frontend/runtime package versions are independent of the
    # Metis release version and are intentionally not rewritten or compared.
    return version, checked


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[1], help="Metis checkout")
    parser.add_argument("--output", type=Path, required=True, help="new or existing empty staging directory")
    parser.add_argument("--report-directory", type=Path, action="append", default=[],
                        help="curated supporting reports; repeat to copy JSON/Markdown from several directories")
    parser.add_argument("--wails", default=shutil.which("wails") or "/Users/yujun2/go/bin/wails", help="Wails CLI")
    parser.add_argument("--go", default=shutil.which("go") or "/Users/yujun2/.local/go-1.26.4/bin/go", help="Go compiler")
    parser.add_argument("--npm", default=shutil.which("npm") or "npm", help="npm executable")
    return parser.parse_args()


def build_package(args: argparse.Namespace, commands: Commands) -> dict[str, object]:
    if sys.platform != "darwin":
        raise PackageError("this package requires macOS native application tools")
    repo = args.repo.expanduser().resolve(strict=True)
    raw_output = args.output.expanduser().absolute()
    if raw_output.is_symlink():
        raise PackageError("staging directory must not be a symlink")
    stage = raw_output.resolve()
    if stage == repo or stage == repo.parent or not stage.name:
        raise PackageError("staging directory must be a dedicated new directory")
    archive = stage.parent / (stage.name + ".zip")
    sidecar = archive.with_name(archive.name + ".sha256")
    verification = stage.parent / (stage.name + ".package-verification.json")
    for path in (archive, sidecar, verification):
        if path.exists() or path.is_symlink():
            raise PackageError("an output archive, checksum or verification file already exists")
    if stage.exists() and (not stage.is_dir() or any(stage.iterdir())):
        raise PackageError("staging directory already exists and is not empty")
    inputs = [path.expanduser().resolve(strict=True) for path in args.report_directory]
    if any(not path.is_dir() or path.is_relative_to(stage) or stage.is_relative_to(path) for path in inputs):
        raise PackageError("report directories must exist outside the staging directory and its parents")
    tools = {"go": args.go, "npm": args.npm, "wails": args.wails,
             "ditto": "/usr/bin/ditto", "codesign": "/usr/bin/codesign"}
    for key, value in tools.items():
        located = shutil.which(value)
        if located is None:
            raise PackageError(f"required executable is unavailable: {key}")
        tools[key] = located
    version, version_sources = checked_release_version(repo)
    docs = [repo / "docs/project-coordinator.md", repo / "docs/coordinator-service-design.md"]
    if any(not path.is_file() for path in docs):
        raise PackageError("current coordinator and service design documents are required")
    excluded = [stage, archive, sidecar, verification]
    initial = source_state(commands, repo, excluded)
    if checked_release_version(repo) != (version, version_sources):
        raise PackageError("release version metadata changed during source validation")
    built_at = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    ldflags = (f"-s -w -X {VERSION_PACKAGE}.Version={version} "
               f"-X {VERSION_PACKAGE}.Commit={initial['commit']} -X {VERSION_PACKAGE}.Date={built_at}")
    environment = dict(os.environ)
    environment["PATH"] = str(Path(tools["go"]).parent) + os.pathsep + environment.get("PATH", "")
    environment.update({"GOOS": "darwin", "GOARCH": "arm64", "GOWORK": "off"})
    stage.mkdir(parents=True, exist_ok=True)
    cli = stage / "metis"
    owns_archive = False
    owns_sidecar = False
    owns_verification = False
    try:
        app = stage / "METIS.app"
        with tempfile.TemporaryDirectory(prefix="metis-local-build-source-") as temporary:
            build_repo = Path(temporary) / "source"
            mirror_details = copy_build_source(commands, repo, build_repo, excluded)
            if source_state(commands, repo, excluded) != initial:
                raise PackageError("source changed while creating the isolated build mirror")
            cli_environment = {**environment, "CGO_ENABLED": "0"}
            runtime_tag = commands.run("derive public CLI build identity", [sys.executable,
                         str(build_repo / "scripts/prepare-desktop-cli.py"), "--repo", str(build_repo),
                         "--print-build-tag", "--commit", str(initial["commit"]), "--date", built_at],
                         build_repo, environment).decode().strip()
            commands.run("build fresh local CLI", [tools["go"], "build", "-mod=readonly", "-trimpath",
                         "-tags", runtime_tag, "-ldflags", ldflags, "-o", str(cli), "./cmd/metis"], build_repo, cli_environment)
            frontend = build_repo / "metis-desktop/frontend"
            commands.run("check native frontend", [tools["npm"], "run", "check"], frontend, environment)
            commands.run("build fresh native frontend", [tools["npm"], "run", "build"], frontend, environment)
            # -s skips only the second frontend build; the independent check/build
            # immediately above is mandatory and cannot be disabled. Wails still
            # regenerates runtime wrappers, exclusively inside this source mirror.
            commands.run("build fresh native application", [tools["wails"], "build", "-s", "-skipbindings",
                         "-m", "-nosyncgomod", "-trimpath", "-platform", "darwin/arm64", "-compiler",
                         tools["go"], "-ldflags", ldflags, "-nocolour"], build_repo / "metis-desktop",
                         {**environment, "CGO_ENABLED": "1"})
            source_app = build_repo / "metis-desktop/build/bin/METIS.app"
            if not source_app.is_dir():
                raise PackageError("native build did not produce METIS.app")
            commands.run("bundle fresh CLI inside native application", [sys.executable,
                         str(build_repo / "scripts/prepare-desktop-cli.py"), "--repo", str(build_repo),
                         "--platform", "darwin/arm64", "--app", str(source_app), "--cli", str(cli),
                         "--go", tools["go"], "--commit", str(initial["commit"]), "--date", built_at,
                         "--source-manifest-sha256", str(initial["source_fingerprint_sha256"])], build_repo, environment)
            # The CLI signature is now sealed by its manifest. Re-sign only the
            # outer bundle, leaving the signed CLI bytes unchanged.
            commands.run("sign native application with bundled CLI", [tools["codesign"], "--force",
                         "--sign", "-", str(source_app)], build_repo)
            commands.run("verify bundled CLI after outer signing", [sys.executable,
                         str(build_repo / "scripts/prepare-desktop-cli.py"), "--repo", str(build_repo),
                         "--platform", "darwin/arm64", "--app", str(source_app),
                         "--verify-only", "--execute-version"], build_repo, environment)
            commands.run("copy native bundle resources", [tools["ditto"], str(source_app), str(app)], build_repo)
            commands.run("verify native bundle signature", [tools["codesign"], "--verify", "--deep", "--strict", str(app)], build_repo)
            if app_symlinks(source_app) != app_symlinks(app):
                raise PackageError("copying the native bundle changed its symbolic links")
            for document in docs:
                shutil.copy2(build_repo / document.relative_to(repo), stage / document.name)
        mirror_details["temporary_source_mirror_removed"] = not Path(temporary).exists()
        launcher = stage / "Launch Metis.command"
        launcher.write_text('#!/bin/zsh\nset -eu\npackage_dir="$(cd "$(dirname "$0")" && pwd)"\n'
                            'exec /usr/bin/open "$package_dir/METIS.app"\n')
        launcher.chmod(0o755)
        reports = copy_reports(inputs, stage)
        final = source_state(commands, repo, excluded)
        if final != initial:
            raise PackageError("source changed during packaging; rebuild from the final reviewed source")
        (stage / "README.txt").write_text(
            f"Metis local runtime build for macOS ARM64\n\n"
            f"Base: v{version}, commit {str(initial['commit'])[:12]}, "
            + ("with uncommitted local changes.\n" if initial["dirty"] else "from a clean local checkout.\n") +
            "This is a local package, not a published release. No version was bumped.\n"
            "Desktop includes its matching CLI inside METIS.app.\n"
            "See the current coordinator and service design documents for usage and limits.\n\n"
            "Open METIS.app directly; no separate CLI installation is required.\n"
            "Launch Metis.command is an optional shortcut. The separate metis executable\n"
            "is included only for optional terminal use. Existing configuration is reused.\n"
            "This package does not\n"
            "replace /Applications/METIS.app or any installed/managed CLI.\n\n"
            "The frontend was checked and freshly built before Wails skipped its duplicate\n"
            "frontend build. CLI, frontend and native application builds used an isolated\n"
            "temporary source mirror, including a copy of frontend dependencies. Wails\n"
            "runtime wrapper generation could not write into the original checkout.\n"
            "Bindings and go.mod synchronization were disabled.\n"
            "The bundle signature is verified locally; this is not Apple notarization.\n"
            "Supporting reports are curated JSON/Markdown copies with credential strings\n"
            "redacted. Historical report results are not an acceptance test of this ZIP.\n"
            "Archive verification checks extracted hashes, permissions, symlinks and signature.\n"
            "Actual query acceptance of the extracted CLI must be performed separately.\n"
            "USD counters are model-catalog estimates rather than billing totals.\n",
            encoding="utf-8",
        )
        executable = app_executable(app)
        cli_hash, app_hash = sha256_file(cli), sha256_file(executable)
        bundled_cli = app / "Contents/Resources/bin/metis"
        bundled_cli_manifest = app / "Contents/Resources/bin/metis-runtime.json"
        bundled_cli_hash = sha256_file(bundled_cli)
        manifest = {"base_version": version, "built_at_utc": built_at, "platform": "darwin/arm64",
                    "validated_version_sources": version_sources,
                    "source": initial, "source_fingerprint_checked_after_build": True,
                    "isolated_build": mirror_details,
                    "uncommitted_diff_hash_scope": "tracked HEAD diff; untracked digests are included in source fingerprint",
                    "cli": {"path": "metis", "sha256": cli_hash, "optional_terminal_use": True},
                    "bundled_cli": {"path": bundled_cli.relative_to(stage).as_posix(),
                                    "sha256": bundled_cli_hash,
                                    "manifest_sha256": sha256_file(bundled_cli_manifest)},
                    "application_executable": {"path": executable.relative_to(stage).as_posix(), "sha256": app_hash},
                    "app_symlinks": app_symlinks(app), "build_steps": list(commands.steps),
                    "supporting_reports": reports, "installed_application_replaced": False,
                    "published": False, "real_query_acceptance": "not performed by this packaging script"}
        write_json(stage / "build-manifest.json", manifest)
        # Reserve the archive name exclusively; ditto may replace only this
        # script's empty reservation, never an existing user's package.
        with archive.open("xb"):
            pass
        owns_archive = True
        commands.run("create local ZIP", [tools["ditto"], "-c", "-k", "--sequesterRsrc", "--keepParent",
                     str(stage), str(archive)], repo)
        with tempfile.TemporaryDirectory(prefix="metis-package-extract-") as temporary:
            extraction = Path(temporary)
            commands.run("extract ZIP for verification", [tools["ditto"], "-x", "-k", str(archive), str(extraction)], repo)
            extracted = extraction / stage.name
            if (sha256_file(extracted / "metis") != cli_hash
                    or sha256_file(app_executable(extracted / "METIS.app")) != app_hash
                    or sha256_file(extracted / bundled_cli.relative_to(stage)) != bundled_cli_hash
                    or sha256_file(extracted / bundled_cli_manifest.relative_to(stage)) != sha256_file(bundled_cli_manifest)):
                raise PackageError("extracted CLI or application hash differs from the fresh build")
            if app_symlinks(extracted / "METIS.app") != app_symlinks(app):
                raise PackageError("ZIP extraction changed native bundle symbolic links")
            if (not os.access(extracted / "metis", os.X_OK)
                    or not os.access(extracted / launcher.name, os.X_OK)
                    or not os.access(app_executable(extracted / "METIS.app"), os.X_OK)):
                raise PackageError("ZIP extraction did not preserve executable permissions")
            commands.run("verify extracted native signature", [tools["codesign"], "--verify", "--deep", "--strict",
                         str(extracted / "METIS.app")], repo)
        archive_hash = sha256_file(archive)
        with sidecar.open("x", encoding="utf-8") as checksum:
            owns_sidecar = True
            checksum.write(f"{archive_hash}  {archive.name}\n")
        evidence = {"archive": str(archive), "sha256": archive_hash,
                    "cli_sha256": cli_hash, "application_executable_sha256": app_hash,
                    "bundled_cli_sha256": bundled_cli_hash, "desktop_requires_external_cli": False,
                    "extracted_hashes_match": True, "extracted_signature_verified": True,
                    "executable_permissions_preserved": True, "bundle_symlinks_preserved": True,
                    "source_fingerprint_sha256": initial["source_fingerprint_sha256"],
                    "temporary_source_mirror_removed": mirror_details["temporary_source_mirror_removed"],
                    "real_query_acceptance": "not performed by this packaging script"}
        with verification.open("x", encoding="utf-8") as report:
            owns_verification = True
            json.dump(evidence, report, indent=2)
            report.write("\n")
        return {"stage": str(stage), "archive": str(archive), "sha256": archive_hash,
                "verification": str(verification), "source_dirty": initial["dirty"],
                "copied_reports": len(reports)}
    except BaseException:
        commands.stop()
        cli.unlink(missing_ok=True)
        if owns_archive:
            archive.unlink(missing_ok=True)
        if owns_sidecar:
            sidecar.unlink(missing_ok=True)
        if owns_verification:
            verification.unlink(missing_ok=True)
        raise


def main() -> int:
    args = parse_args()
    commands = Commands()
    def interrupted(_signum: int, _frame: object) -> None:
        raise KeyboardInterrupt
    previous = signal.signal(signal.SIGTERM, interrupted)
    try:
        print(json.dumps(build_package(args, commands), ensure_ascii=False))
        return 0
    except KeyboardInterrupt:
        commands.stop()
        print("local packaging interrupted; owned build processes stopped and temporary stage CLI removed", file=sys.stderr)
        return 130
    except (PackageError, OSError, ValueError, subprocess.TimeoutExpired) as error:
        commands.stop()
        print(f"local packaging failed: {error}", file=sys.stderr)
        return 1
    finally:
        signal.signal(signal.SIGTERM, previous)


if __name__ == "__main__":
    raise SystemExit(main())
