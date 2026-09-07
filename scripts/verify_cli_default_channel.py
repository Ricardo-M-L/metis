#!/usr/bin/env python3
"""Read-only CI oracle for cli-stable-v1; never downloads or installs a binary.

Independent of the Go/Bash/PowerShell resolvers under test. A disposable Python
child reads each bounded metadata response, so the parent enforces one 30-second
deadline even if DNS/TLS or a trickling response stalls. No token is printed or
put in a subprocess argument. Only the fixed GitHub metadata origin is used.
"""

import argparse
from datetime import datetime, timezone
import json
import math
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.request


MAX_BYTES = 8 * 1024 * 1024
MAX_PAGES = 10
BUDGET_SECONDS = 30
TAG = re.compile(r"v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\Z")
CLI_ASSETS = tuple(
    f"metis-{platform}-{arch}{'.zip' if platform == 'windows' else '.tar.gz'}{suffix}"
    for platform in ("darwin", "linux", "windows")
    for arch in ("amd64", "arm64") for suffix in ("", ".sha256")
)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def version(tag):
    match = TAG.fullmatch(tag) if isinstance(tag, str) else None
    return tuple(map(int, match.groups())) if match else None


def validate_repo(repository):
    require(isinstance(repository, str) and re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository),
            "invalid repository")


def positive_integer_number(value):
    return ((type(value) is int and value > 0) or
            (type(value) is float and math.isfinite(value) and value > 0 and value.is_integer()))


def select_highest(releases, repository):
    validate_repo(repository)
    highest, selected = None, ""
    for release in releases:
        if not isinstance(release, dict):
            continue
        tag = release.get("tag_name")
        parsed = version(tag)
        if parsed is None or release.get("draft") is not False or release.get("prerelease") is not False:
            continue
        assets = release.get("assets")
        if not isinstance(assets, list):
            continue
        valid = True
        for name in CLI_ASSETS:
            matches = [asset for asset in assets if isinstance(asset, dict) and asset.get("name") == name]
            if len(matches) != 1:
                valid = False
                break
            asset = matches[0]
            expected_url = f"https://github.com/{repository}/releases/download/{tag}/{name}"
            if (not positive_integer_number(asset.get("size")) or
                    asset.get("state") != "uploaded" or asset.get("browser_download_url") != expected_url):
                valid = False
                break
        if valid and (highest is None or parsed > highest):
            highest, selected = parsed, tag
    return selected


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def read_page(repository, page, timeout):
    validate_repo(repository)
    require(1 <= page <= MAX_PAGES and 0 < timeout <= BUDGET_SECONDS, "invalid metadata request bounds")
    url = f"https://api.github.com/repos/{repository}/releases?per_page=100&page={page}"
    headers = {"Accept": "application/vnd.github+json", "User-Agent": "metis-cli-channel-verifier"}
    token = os.environ.get("METIS_GITHUB_TOKEN") or os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN")
    if token:
        require("\r" not in token and "\n" not in token, "invalid authentication value")
        headers["Authorization"] = "Bearer " + token
    opener = urllib.request.build_opener(NoRedirect())
    with opener.open(urllib.request.Request(url, headers=headers), timeout=timeout) as response:
        require(response.status == 200, "metadata HTTP failure")
        body = response.read(MAX_BYTES + 1)
    require(len(body) <= MAX_BYTES, "metadata response exceeds 8 MiB")
    return body


def fetch_page(repository, page, remaining):
    result = subprocess.run(
        [sys.executable, str(Path(__file__).resolve()), "_page", "--repository", repository,
         "--page", str(page), "--timeout", str(remaining)],
        capture_output=True, timeout=remaining,
    )
    require(result.returncode == 0, "metadata request failed; no shared-latest fallback")
    return result.stdout


def discover(repository, *, fetch=fetch_page, clock=time.monotonic):
    validate_repo(repository)
    deadline = clock() + BUDGET_SECONDS
    releases = []
    for page in range(1, MAX_PAGES + 1):
        remaining = deadline - clock()
        require(remaining > 0, "CLI metadata deadline exceeded")
        raw = fetch(repository, page, remaining)
        require(clock() <= deadline, "CLI metadata deadline exceeded")
        require(len(raw) <= MAX_BYTES, "metadata response exceeds 8 MiB")
        parsed = json.loads(raw)
        require(isinstance(parsed, list) and len(parsed) <= 100, "metadata page must contain at most 100 releases")
        releases.extend(parsed)
        if len(parsed) < 100:
            selected = select_highest(releases, repository)
            require(selected, "no complete stable CLI release found")
            require(clock() <= deadline, "CLI metadata deadline exceeded")
            return {"schema_version": 1, "repository": repository, "selected_tag": selected,
                    "pages_scanned": page, "observed_at": datetime.now(timezone.utc).isoformat()}
    raise ValueError("full tenth metadata page; refusing a partial result")


def supports_default_check(tag, prerelease):
    parsed = version(tag)
    require(parsed is not None and type(prerelease) is bool, "invalid release gate metadata")
    return parsed >= (0, 4, 52) and not prerelease


def verify_install(repository, minimum_tag, actual_tag, before, after):
    validate_repo(repository)
    minimum, actual = version(minimum_tag), version(actual_tag)
    require(minimum is not None and actual is not None and actual >= minimum,
            "default install is older than the release under verification")
    for snapshot in (before, after):
        require(isinstance(snapshot, dict) and type(snapshot.get("schema_version")) is int and snapshot["schema_version"] == 1 and
                snapshot.get("repository") == repository and version(snapshot.get("selected_tag")) is not None,
                "invalid independent CLI channel snapshot")
    expected = {before["selected_tag"], after["selected_tag"]}
    require(actual_tag in expected, "default install does not match either independent CLI latest snapshot")
    return {"minimum_tag": minimum_tag, "actual_tag": actual_tag,
            "expected_before": before["selected_tag"], "expected_after": after["selected_tag"], "passed": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("supports", "snapshot", "verify", "_page"))
    parser.add_argument("--repository")
    parser.add_argument("--tag")
    parser.add_argument("--prerelease", choices=("true", "false"), default="false")
    parser.add_argument("--actual")
    parser.add_argument("--before", type=Path)
    parser.add_argument("--after", type=Path)
    parser.add_argument("--page", type=int)
    parser.add_argument("--timeout", type=float)
    args = parser.parse_args()
    if args.command == "supports":
        print(str(supports_default_check(args.tag, args.prerelease == "true")).lower())
    elif args.command == "snapshot":
        print(json.dumps(discover(args.repository), sort_keys=True))
    elif args.command == "verify":
        require(args.before and args.after, "verification requires before and after snapshots")
        print(json.dumps(verify_install(args.repository, args.tag, args.actual,
                                       json.loads(args.before.read_text()), json.loads(args.after.read_text())), sort_keys=True))
    else:
        sys.stdout.buffer.write(read_page(args.repository, args.page, args.timeout))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # Neither HTTP response bodies nor credentials from inherited env appear
        # in diagnostics. The parent distinguishes a failed probe from absence.
        detail = str(error) if isinstance(error, ValueError) else "metadata request failed"
        print("verify-cli-default-channel: " + type(error).__name__ + ": " + detail, file=sys.stderr)
        sys.exit(1)
