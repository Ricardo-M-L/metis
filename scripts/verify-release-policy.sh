#!/bin/sh

# Static guard for the immutable release contract. It deliberately checks only
# public workflow files and never reads local signing material.

set -eu

fail() {
	printf 'verify-release-policy: %s\n' "$*" >&2
	exit 1
}

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
stage_workflow=$repo_root/.github/workflows/release.yml
verify_workflow=$repo_root/.github/workflows/release-published.yml

[ -f "$stage_workflow" ] || fail "missing release.yml"
[ -f "$verify_workflow" ] || fail "missing release-published.yml"

grep -q 'Stage immutable release draft' "$stage_workflow" || fail "tag workflow does not stage a draft"
grep -q 'expected=16' "$stage_workflow" || fail "draft inventory is not the 16 non-macOS assets"
grep -q 'metis-desktop-linux-amd64-' "$stage_workflow" || fail "Linux Desktop asset is not staged"
grep -q 'metis-desktop-windows-amd64-' "$stage_workflow" || fail "Windows Desktop asset is not staged"
grep -Fq '[System.IO.File]::WriteAllText' "$stage_workflow" || \
	fail "Windows Desktop checksum sidecar is not written with explicit LF encoding"

if grep -q 'gh release edit .*--draft=false' "$stage_workflow"; then
	fail "tag workflow must not publish a draft"
fi
if grep -A8 'name: Download.*Desktop artifact' "$stage_workflow" | grep -q 'darwin-universal'; then
	fail "CI ad-hoc macOS artifact must not be staged in the Release draft"
fi

grep -q 'types: \[published\]' "$verify_workflow" || fail "published-release verification trigger is missing"
grep -q 'workflow_dispatch:' "$verify_workflow" || fail "published-release manual recheck trigger is missing"
grep -q 'exactly the 20 immutable assets' "$verify_workflow" || fail "published inventory guard is missing"
grep -Fq "sed 's/\\r\$//'" "$verify_workflow" || fail "checksum verification is not CRLF-safe"
grep -q 'Authority=Developer ID Application:' "$verify_workflow" || fail "Developer ID verification is missing"
grep -q 'xcrun stapler validate' "$verify_workflow" || fail "staple verification is missing"
grep -q 'spctl --assess' "$verify_workflow" || fail "Gatekeeper verification is missing"
grep -q 'app=build/bin/METIS.app' "$stage_workflow" || fail "macOS build is not normalized to METIS.app"
grep -q 'bridge_app=build/bin/metis-desktop.app' "$stage_workflow" || fail "v0.4.42 updater bridge is missing"
grep -q 'app=unpacked/metis-desktop.app' "$verify_workflow" || fail "published updater ZIP bridge is not verified"
grep -q 'CFBundleDisplayName).*METIS' "$verify_workflow" || fail "published macOS display name is not verified as METIS"

printf '%s\n' 'verify-release-policy: immutable draft and published-release gates verified'
