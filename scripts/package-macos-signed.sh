#!/bin/sh

# Build the release-grade METIS Desktop artifacts with the local Developer ID
# Application identity. Raw Apple credentials remain outside this script and
# the build tree: the private sentinel is checked only for file metadata, and
# notarization uses a pre-created Keychain profile.

set -eu

fail() {
	printf 'package-macos-signed: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat >&2 <<'EOF'
usage: scripts/package-macos-signed.sh [--notary-profile PROFILE]
EOF
	exit 2
}

notary_profile=metis-notary
while [ "$#" -gt 0 ]; do
	case "$1" in
	--notary-profile)
		[ "$#" -ge 2 ] || usage
		[ -n "$2" ] || usage
		notary_profile=$2
		shift 2
		;;
	-h | --help)
		usage
		;;
	*)
		usage
		;;
	esac
done

[ "$(uname -s)" = Darwin ] || fail "macOS is required"

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
credentials_rel=.private/apple/teamid.txt
credentials_file=$repo_root/$credentials_rel

[ -f "$credentials_file" ] || fail "missing private signing reference: $credentials_rel"
[ ! -L "$credentials_file" ] || fail "$credentials_rel must not be a symbolic link"
[ "$(stat -f '%Lp' "$credentials_file")" = 600 ] || fail "$credentials_rel must have mode 0600"
git -C "$repo_root" check-ignore -q "$credentials_rel" || fail "$credentials_rel is not ignored by Git"
if git -C "$repo_root" ls-files --error-unmatch "$credentials_rel" >/dev/null 2>&1; then
	fail "$credentials_rel is tracked by Git; refusing to package"
fi

for command_name in wails codesign security ditto hdiutil shasum unzip xcrun spctl; do
	command -v "$command_name" >/dev/null 2>&1 || fail "missing required command: $command_name"
done
xcrun --find notarytool >/dev/null 2>&1 || fail "missing required Xcode tool: notarytool"
xcrun --find stapler >/dev/null 2>&1 || fail "missing required Xcode tool: stapler"

identities=$(security find-identity -v -p codesigning 2>/dev/null | awk '/"Developer ID Application:/{print $2}')
identity_count=$(printf '%s\n' "$identities" | sed '/^$/d' | wc -l | tr -d ' ')
[ "$identity_count" = 1 ] || fail "expected exactly one valid Developer ID Application identity, found $identity_count"
identity=$(printf '%s\n' "$identities" | sed -n '1p')

desktop_dir=$repo_root/metis-desktop
app_path=$desktop_dir/build/bin/METIS.app
output_dir=$desktop_dir/release-dist-local
archive=$output_dir/metis-desktop-darwin-universal.zip
dmg=$output_dir/metis-desktop-darwin-universal.dmg
dmg_stage=$output_dir/dmg-stage
zip_stage=$output_dir/zip-stage

build_updater_bridge_archive() {
	rm -rf "$zip_stage"
	mkdir -p "$zip_stage"
	# v0.4.42 requires this legacy updater-ZIP root. The signed bundle's
	# metadata and the user-facing DMG already use METIS.
	ditto "$app_path" "$zip_stage/metis-desktop.app"
	rm -f "$archive"
	ditto -c -k --sequesterRsrc --keepParent "$zip_stage/metis-desktop.app" "$archive"
	unzip -tq "$archive"
	rm -rf "$zip_stage"
}

printf '%s\n' "Building METIS Desktop for darwin/universal..."
(
	cd "$desktop_dir"
	wails build -clean -s -trimpath -skipbindings -platform darwin/universal
)
[ -d "$app_path" ] || fail "Wails did not create $app_path"
if find "$app_path" \( -type d -name .private -o -type f -name teamid.txt -o \
	-type f -name '*.p12' -o -type f -name '*.p8' \) -print -quit | grep -q .; then
	fail "application bundle contains forbidden signing material"
fi
if find "$app_path" -type f \
	-exec grep -aEq -- '-----BEGIN ([A-Z0-9 ]+ )?PRIVATE KEY-----' {} \; \
	-print -quit | grep -q .; then
	fail "application bundle contains private-key content"
fi

printf '%s\n' "Signing with the local Developer ID Application identity..."
codesign --force --deep --options runtime --timestamp --sign "$identity" "$app_path"
codesign --verify --deep --strict --verbose=2 "$app_path"
codesign -dv --verbose=4 "$app_path" 2>&1 | grep -q '^Authority=Developer ID Application:' || \
	fail "application is not signed by a Developer ID Application identity"

mkdir -p "$output_dir"
rm -f "$archive" "$archive.sha256" "$dmg" "$dmg.sha256"
build_updater_bridge_archive

printf '%s\n' "Submitting the signed archive for Apple notarization..."
xcrun notarytool submit "$archive" --keychain-profile "$notary_profile" --wait
xcrun stapler staple "$app_path"
xcrun stapler validate "$app_path"
build_updater_bridge_archive

rm -rf "$dmg_stage"
mkdir -p "$dmg_stage"
ditto "$app_path" "$dmg_stage/METIS.app"
ln -s /Applications "$dmg_stage/Applications"
hdiutil create -quiet -ov -format UDZO -volname "METIS" -srcfolder "$dmg_stage" "$dmg"
hdiutil verify "$dmg"
rm -rf "$dmg_stage"

printf '%s\n' "Signing the DMG container with the local Developer ID Application identity..."
codesign --force --timestamp --sign "$identity" "$dmg"
codesign --verify --verbose=2 "$dmg"
codesign -dv --verbose=4 "$dmg" 2>&1 | grep -q '^Authority=Developer ID Application:' || \
	fail "DMG is not signed by a Developer ID Application identity"

printf '%s\n' "Submitting the installer DMG for Apple notarization..."
xcrun notarytool submit "$dmg" --keychain-profile "$notary_profile" --wait
xcrun stapler staple "$dmg"
xcrun stapler validate "$dmg"
spctl --assess --type execute --verbose=2 "$app_path"
spctl --assess --type open --context context:primary-signature --verbose=2 "$dmg"

(cd "$output_dir" && shasum -a 256 "$(basename "$archive")" > "$(basename "$archive").sha256")
(cd "$output_dir" && shasum -a 256 "$(basename "$dmg")" > "$(basename "$dmg").sha256")
(cd "$output_dir" && shasum -a 256 -c "$(basename "$archive").sha256") >/dev/null
(cd "$output_dir" && shasum -a 256 -c "$(basename "$dmg").sha256") >/dev/null
printf 'Created %s\nCreated %s\n' "$archive" "$dmg"
