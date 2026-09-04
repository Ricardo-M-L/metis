#!/bin/sh

# Static guard for the immutable release contract. It checks tracked public
# files and signing-script structure, but never reads local signing material.

set -eu

fail() {
	printf 'verify-release-policy: %s\n' "$*" >&2
	exit 1
}

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
stage_workflow=$repo_root/.github/workflows/release.yml
verify_workflow=$repo_root/.github/workflows/release-published.yml
package_script=$repo_root/scripts/package-macos-signed.sh

provider_token_pattern='[0-9a-fA-F]{32}\.[A-Za-z0-9_-]{16,}'
provider_token_exclude=':(exclude)scripts/verify-release-policy.sh'
provider_token_scan_status=0
git -C "$repo_root" grep --quiet --text --extended-regexp "$provider_token_pattern" -- \
	. "$provider_token_exclude" || \
	provider_token_scan_status=$?
case "$provider_token_scan_status" in
0)
	provider_token_files=$(git -C "$repo_root" grep --text --files-with-matches \
		--extended-regexp "$provider_token_pattern" -- . "$provider_token_exclude") || \
		fail "could not list tracked files containing a suspected provider token"
	printf '%s\n' "$provider_token_files" | while IFS= read -r provider_token_file; do
		[ -n "$provider_token_file" ] && \
			printf 'verify-release-policy: suspected provider token in tracked file: %s\n' \
				"$provider_token_file" >&2
	done
	fail "tracked source tree contains a suspected provider token"
	;;
1)
	;;
*)
	fail "tracked source tree provider-token scan failed"
	;;
esac

[ -f "$stage_workflow" ] || fail "missing release.yml"
[ -f "$verify_workflow" ] || fail "missing release-published.yml"
[ -f "$package_script" ] || fail "missing package-macos-signed.sh"
[ -x "$package_script" ] || fail "package-macos-signed.sh is not executable"

grep -Fq 'credentials_rel=.private/apple/teamid.txt' "$package_script" || \
	fail "macOS packaging does not use the private signing sentinel"
grep -Fq "stat -f '%Lp' \"\$credentials_file\"" "$package_script" || \
	fail "macOS packaging does not require mode 0600 for the private signing sentinel"
grep -Fq "check-ignore -q \"\$credentials_rel\"" "$package_script" || \
	fail "macOS packaging does not require the private signing sentinel to be ignored"
grep -Fq "ls-files --error-unmatch \"\$credentials_rel\"" "$package_script" || \
	fail "macOS packaging does not reject a tracked private signing sentinel"
grep -Fq 'PRIVATE KEY-----' "$package_script" || \
	fail "macOS application bundle private-key content gate is missing"
grep -Fq 'security find-identity -v -p codesigning' "$package_script" || \
	fail "macOS Developer ID Application signing gate is missing"
grep -Fq "codesign --force --deep --options runtime --timestamp --sign \"\$identity\" \"\$app_path\"" \
	"$package_script" || \
	fail "macOS hardened-runtime timestamp signing gate is missing"
grep -Fq "codesign --force --timestamp --sign \"\$identity\" \"\$dmg\"" \
	"$package_script" || fail "macOS DMG Developer ID signing gate is missing"
grep -Fq 'notary_profile=metis-notary' "$package_script" || \
	fail "macOS notarization keychain profile default is missing"
grep -Fq "notarytool submit \"\$archive\" --keychain-profile \"\$notary_profile\" --wait" \
	"$package_script" || fail "macOS ZIP notarization gate is missing"
grep -Fq "notarytool submit \"\$dmg\" --keychain-profile \"\$notary_profile\" --wait" \
	"$package_script" || fail "macOS DMG notarization gate is missing"
grep -Fq "stapler staple \"\$app_path\"" "$package_script" || \
	fail "macOS application stapling gate is missing"
grep -Fq "stapler validate \"\$app_path\"" "$package_script" || \
	fail "macOS application staple validation gate is missing"
grep -Fq "stapler staple \"\$dmg\"" "$package_script" || \
	fail "macOS DMG stapling gate is missing"
grep -Fq "stapler validate \"\$dmg\"" "$package_script" || \
	fail "macOS DMG staple validation gate is missing"
grep -Fq 'spctl --assess --type execute' "$package_script" || \
	fail "macOS application Gatekeeper gate is missing"
grep -Fq 'spctl --assess --type open' "$package_script" || \
	fail "macOS DMG Gatekeeper gate is missing"
grep -Fq 'metis-desktop-darwin-universal.zip' "$package_script" || \
	fail "macOS ZIP output is missing"
grep -Fq 'metis-desktop-darwin-universal.dmg' "$package_script" || \
	fail "macOS DMG output is missing"
grep -Fq "shasum -a 256 \"\$(basename \"\$archive\")\" >" "$package_script" || \
	fail "macOS ZIP SHA-256 generation gate is missing"
grep -Fq "shasum -a 256 \"\$(basename \"\$dmg\")\" >" "$package_script" || \
	fail "macOS DMG SHA-256 generation gate is missing"
grep -Fq 'shasum -a 256 -c' "$package_script" || \
	fail "macOS SHA-256 verification gate is missing"
if grep -Eq 'security[[:space:]]+import|notarytool[[:space:]]+store-credentials|--apple-id|--password|--team-id|--key-id|--issuer|--key([[:space:]]|=)' \
	"$package_script"; then
	fail "macOS packaging script must use existing keychain identities and profiles only"
fi
if grep -Fq 'Notarization skipped' "$package_script"; then
	fail "macOS release packaging must not permit skipping notarization"
fi

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
