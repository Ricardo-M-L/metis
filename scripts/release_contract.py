#!/usr/bin/env python3
"""Fail-closed release-channel/inventory checks. No network or mutations.

The registry is trusted repository release tooling, never a workflow input or a
release-body claim. An absent entry means the full stable/20-asset contract.
GitHub does not return make_latest on GET: callers must supply an independent
/releases/latest response to prove that a CLI preview was not promoted.
"""

import argparse
import hashlib
import json
from pathlib import Path
import re
import sys


TAG_RE = re.compile(r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\Z")
CLI_ARCHIVES = (
    "metis-darwin-amd64.tar.gz", "metis-darwin-arm64.tar.gz",
    "metis-linux-amd64.tar.gz", "metis-linux-arm64.tar.gz",
    "metis-windows-amd64.zip", "metis-windows-arm64.zip",
)
DESKTOP_ARCHIVES = (
    "metis-desktop-darwin-universal.dmg", "metis-desktop-darwin-universal.zip",
    "metis-desktop-linux-amd64.tar.gz", "metis-desktop-windows-amd64.zip",
)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def validate_tag(tag):
    require(isinstance(tag, str) and TAG_RE.fullmatch(tag), "invalid vX.Y.Z release tag")


def release_plan(registry, tag):
    validate_tag(tag)
    require(isinstance(registry, dict) and set(registry) == {"schema_version", "releases"},
            "invalid registry schema")
    require(type(registry["schema_version"]) is int and registry["schema_version"] == 1,
            "unsupported registry schema version")
    releases = registry["releases"]
    require(isinstance(releases, dict), "registry releases must be an object")
    for registered_tag, entry in releases.items():
        validate_tag(registered_tag)
        require(isinstance(entry, dict) and set(entry) == {
            "channel", "prerelease", "make_latest", "reason"}, "invalid registry entry schema")
        require(entry["channel"] == "cli-only-prerelease", "invalid registry channel")
        require(entry["prerelease"] is True and entry["make_latest"] is False,
                "CLI-only registry requires prerelease=true and make_latest=false")
        require(isinstance(entry["reason"], str) and entry["reason"].strip(), "missing registry reason")
    preview = tag in releases
    archives = CLI_ARCHIVES if preview else CLI_ARCHIVES + DESKTOP_ARCHIVES
    return {
        "tag": tag,
        "channel": "cli-only-prerelease" if preview else "stable",
        "prerelease": preview,
        "make_latest": False if preview else None,
        "assets": sorted(name for archive in archives for name in (archive, archive + ".sha256")),
    }


def verify_release(registry, tag, metadata, latest, *, phase, complete=True):
    plan = release_plan(registry, tag)
    require(phase in ("draft", "published"), "invalid release phase")
    require(isinstance(metadata, dict) and metadata.get("tag_name") == tag, "release tag mismatch")
    require(metadata.get("draft") is (phase == "draft"),
            "release draft state mismatch; published releases are immutable")
    require(metadata.get("prerelease") is plan["prerelease"], "release prerelease state mismatch")
    require(isinstance(latest, dict) and isinstance(latest.get("tag_name"), str),
            "missing independent latest release evidence")
    validate_tag(latest["tag_name"])
    if plan["channel"] == "cli-only-prerelease":
        require(latest["tag_name"] != tag, "CLI-only prerelease must not be latest")
    assets = metadata.get("assets")
    require(isinstance(assets, list), "missing release asset inventory")
    names = []
    for asset in assets:
        require(isinstance(asset, dict) and isinstance(asset.get("name"), str), "invalid release asset")
        require(asset.get("state") == "uploaded", "asset not fully uploaded")
        require(type(asset.get("size")) is int and asset["size"] > 0, "empty or invalid asset size")
        names.append(asset["name"])
    require(len(names) == len(set(names)), "duplicate release asset")
    expected = set(plan["assets"])
    require(set(names) == expected if complete else set(names) <= expected,
            "release inventory mismatch: expected " + str(len(expected)) + " channel-specific assets")
    return plan


def verify_dist(directory, assets):
    directory = Path(directory)
    entries = list(directory.iterdir())
    require({entry.name for entry in entries} == set(assets), "downloaded inventory mismatch")
    require(all(entry.is_file() and not entry.is_symlink() for entry in entries),
            "downloaded inventory contains non-regular file")
    for name in assets:
        if not name.endswith(".sha256"):
            continue
        archive = name[:-7]
        # GitHub's Windows sidecars may use CRLF; only one exact basename is
        # accepted, so a manifest cannot ask the checker to read another path.
        manifest = (directory / name).read_text(encoding="utf-8")
        match = re.fullmatch(r"([0-9a-f]{64})[ \t]+" + re.escape(archive) + r"\n?", manifest)
        require(match is not None, "invalid checksum sidecar: " + name)
        require(file_sha256(directory / archive) == match[1], "checksum mismatch: " + archive)


def file_sha256(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def verify_build_run(tag, repository, run_id, source_sha, run, workflow, artifacts):
    """Bind an immutable artifact ID to a successful exact-source Release run."""
    validate_tag(tag)
    require(isinstance(repository, str) and re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository),
            "invalid expected repository")
    require(type(run_id) is int and run_id > 0, "invalid expected build run ID")
    require(isinstance(source_sha, str) and re.fullmatch(r"[0-9a-f]{40}", source_sha),
            "invalid source commit SHA")
    require(isinstance(run, dict) and run.get("id") == run_id, "build run ID mismatch")
    require(run.get("status") == "completed" and run.get("conclusion") == "success",
            "Release build run must be completed successfully")
    require(run.get("event") in ("push", "workflow_dispatch"), "untrusted build run event")
    require(run.get("head_sha") == source_sha, "build run head SHA differs from tag commit")
    release_path = ".github/workflows/release.yml"
    require(isinstance(workflow, dict) and workflow.get("path") == release_path and
            workflow.get("state") == "active", "Release workflow identity/state mismatch")
    require(type(workflow.get("id")) is int and workflow["id"] > 0 and
            run.get("workflow_id") == workflow["id"] and run.get("path") == release_path,
            "build run is not the trusted Release workflow")
    repo = run.get("repository")
    head_repo = run.get("head_repository")
    require(isinstance(repo, dict) and isinstance(head_repo, dict), "missing build repository identity")
    require(repo.get("full_name") == repository and head_repo.get("full_name") == repository and
            type(repo.get("id")) is int and repo["id"] > 0 and head_repo.get("id") == repo["id"],
            "build repository or head repository mismatch")
    require(isinstance(artifacts, dict) and isinstance(artifacts.get("artifacts"), list),
            "missing build artifacts")
    entries = artifacts["artifacts"]
    require(type(artifacts.get("total_count")) is int and artifacts["total_count"] == len(entries),
            "incomplete build artifact listing")
    require(all(isinstance(entry, dict) for entry in entries), "invalid build artifact entry")
    matches = [entry for entry in entries if entry.get("name") == "metis-release-" + tag]
    require(len(matches) == 1, "missing or duplicate exact-tag build artifact")
    artifact = matches[0]
    require(type(artifact.get("id")) is int and artifact["id"] > 0 and
            artifact.get("expired") is False and type(artifact.get("size_in_bytes")) is int and
            artifact["size_in_bytes"] > 0, "build artifact is invalid, empty, or expired")
    artifact_run = artifact.get("workflow_run")
    require(isinstance(artifact_run, dict) and artifact_run.get("id") == run_id and
            artifact_run.get("head_sha") == source_sha and
            artifact_run.get("repository_id") == repo["id"] and
            artifact_run.get("head_repository_id") == repo["id"], "build artifact run/source mismatch")
    return {"run_id": run_id, "source_sha": source_sha, "repository": repository,
            "workflow_id": workflow["id"], "artifact_id": artifact["id"], "tag": tag}


def verify_build_bytes(workflow_dist, draft_dist, assets):
    # A replaced binary with a freshly matching sidecar passes self-consistency,
    # but must not pass provenance: compare all 12 bytesets to the trusted build.
    verify_dist(workflow_dist, assets)
    verify_dist(draft_dist, assets)
    hashes = {}
    for name in assets:
        expected = file_sha256(Path(workflow_dist) / name)
        require(file_sha256(Path(draft_dist) / name) == expected,
                "draft differs from trusted build artifact bytes: " + name)
        hashes[name] = expected
    return hashes


def no_duplicate_keys(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "duplicate JSON key: " + key)
        result[key] = value
    return result


def load_json(path):
    return json.loads(Path(path).read_text(encoding="utf-8"), object_pairs_hook=no_duplicate_keys)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("plan", "verify", "provenance"))
    parser.add_argument("--registry", type=Path,
                        default=Path(__file__).resolve().parent.parent / ".github/cli-only-releases.json")
    parser.add_argument("--tag", required=True)
    parser.add_argument("--metadata", type=Path)
    parser.add_argument("--latest", type=Path)
    parser.add_argument("--phase", choices=("draft", "published"), default="published")
    parser.add_argument("--allow-partial-draft", action="store_true")
    parser.add_argument("--dist", type=Path)
    parser.add_argument("--workflow-dist", type=Path)
    parser.add_argument("--run", type=Path)
    parser.add_argument("--workflow", type=Path)
    parser.add_argument("--artifacts", type=Path)
    parser.add_argument("--repository")
    parser.add_argument("--run-id", type=int)
    parser.add_argument("--source-sha")
    args = parser.parse_args()
    registry = load_json(args.registry)
    plan = release_plan(registry, args.tag)
    if args.command == "provenance":
        require(plan["channel"] == "cli-only-prerelease", "provenance publication requires registered CLI preview")
        require(args.run and args.workflow and args.artifacts and args.repository and args.run_id and args.source_sha,
                "provenance requires run/workflow/artifacts/repository/run-id/source-sha")
        require(not (args.metadata or args.latest or args.allow_partial_draft),
                "provenance does not accept release-verification arguments")
        evidence = verify_build_run(args.tag, args.repository, args.run_id, args.source_sha,
                                    load_json(args.run), load_json(args.workflow), load_json(args.artifacts))
        require(bool(args.workflow_dist) == bool(args.dist), "provenance byte comparison requires both directories")
        if args.dist:
            evidence["asset_sha256"] = verify_build_bytes(args.workflow_dist, args.dist, plan["assets"])
        print(json.dumps(evidence, sort_keys=True))
        return
    require(not any((args.workflow_dist, args.run, args.workflow, args.artifacts, args.repository,
                     args.run_id, args.source_sha)), "build provenance arguments require provenance command")
    if args.command == "verify":
        require(args.metadata and args.latest, "verify requires --metadata and --latest")
        require(not args.allow_partial_draft or args.phase == "draft", "only drafts may be partial")
        plan = verify_release(registry, args.tag, load_json(args.metadata), load_json(args.latest),
                              phase=args.phase, complete=not args.allow_partial_draft)
        if args.dist:
            require(not args.allow_partial_draft, "downloaded inventory must be complete")
            verify_dist(args.dist, plan["assets"])
    else:
        require(not (args.metadata or args.latest or args.dist or args.allow_partial_draft),
                "plan does not accept verification-only arguments")
    print(json.dumps(plan, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError) as error:
        print("release-contract: " + str(error), file=sys.stderr)
        sys.exit(1)
