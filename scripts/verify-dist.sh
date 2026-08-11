#!/bin/sh

# Verify release metadata and the complete, cross-platform distribution set.
# The script is intentionally POSIX sh so the same checks can run locally and
# on GitHub's Linux/macOS runners.

set -eu

fail() {
	printf 'verify-dist: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat >&2 <<'EOF'
usage: scripts/verify-dist.sh [--repo PATH] [--tag vX.Y.Z] [--metadata-only]
EOF
	exit 2
}

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
release_tag=
release_tag_provided=false
metadata_only=false

while [ "$#" -gt 0 ]; do
	case "$1" in
	--repo)
		[ "$#" -ge 2 ] || usage
		repo_root=$(CDPATH= cd -- "$2" && pwd) || fail "repository does not exist: $2"
		shift 2
		;;
	--tag)
		[ "$#" -ge 2 ] || usage
		release_tag=$2
		release_tag_provided=true
		shift 2
		;;
	--metadata-only)
		metadata_only=true
		shift
		;;
	-h | --help)
		usage
		;;
	*)
		usage
		;;
	esac
done

if [ "$release_tag_provided" = true ]; then
	printf '%s\n' "$release_tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || \
		fail "release tag must be a stable semantic version in vX.Y.Z form: $release_tag"
fi

version_file=$repo_root/VERSION
internal_file=$repo_root/internal/version/version.go
npm_file=$repo_root/install/npm/package.json

[ -f "$version_file" ] || fail "missing VERSION"
[ -f "$internal_file" ] || fail "missing internal/version/version.go"
[ -f "$npm_file" ] || fail "missing install/npm/package.json"

version=$(sed -n '1p' "$version_file")
[ -n "$version" ] || fail "VERSION is empty"
printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?$' || \
	fail "VERSION is not a supported semantic version: $version"
[ "$(sed -n '$=' "$version_file")" = "1" ] || fail "VERSION must contain exactly one line"

internal_versions=$(sed -nE 's/^[[:space:]]*Version[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' "$internal_file")
[ "$(printf '%s\n' "$internal_versions" | sed '/^$/d' | wc -l | tr -d ' ')" = "1" ] || \
	fail "could not identify exactly one internal Version default"
internal_version=$internal_versions

npm_versions=$(sed -nE 's/^[[:space:]]*"version"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' "$npm_file")
[ "$(printf '%s\n' "$npm_versions" | sed '/^$/d' | wc -l | tr -d ' ')" = "1" ] || \
	fail "could not identify exactly one npm package version"
npm_version=$npm_versions

[ "$internal_version" = "$version" ] || \
	fail "internal version $internal_version does not match VERSION $version"
[ "$npm_version" = "$version" ] || \
	fail "npm version $npm_version does not match VERSION $version"

expected_tag=v$version
if [ "$release_tag_provided" = true ] && [ "$release_tag" != "$expected_tag" ]; then
	fail "release tag $release_tag does not match $expected_tag"
fi

printf 'verify-dist: metadata matches %s\n' "$expected_tag"
[ "$metadata_only" = true ] && exit 0

dist_dir=$repo_root/dist
[ -d "$dist_dir" ] || fail "missing dist directory; run make dist first"

archives='metis-darwin-amd64.tar.gz
metis-darwin-arm64.tar.gz
metis-linux-amd64.tar.gz
metis-linux-arm64.tar.gz
metis-windows-amd64.zip
metis-windows-arm64.zip'

# `-mindepth`/`-maxdepth` are GNU extensions and are absent from macOS's
# BSD find. Prune every direct child after visiting it to get a portable,
# non-recursive count (including dotfiles).
entry_count=$(find "$dist_dir" ! -path "$dist_dir" -prune -print | wc -l | tr -d ' ')
file_count=$(find "$dist_dir" ! -path "$dist_dir" -prune -type f -print | wc -l | tr -d ' ')
[ "$entry_count" = "12" ] || fail "dist must contain exactly 12 entries, found $entry_count"
[ "$file_count" = "12" ] || fail "all 12 dist entries must be regular files, found $file_count files"

verify_tmp=$(mktemp -d "${TMPDIR:-/tmp}/metis-verify-dist.XXXXXX")
cleanup() {
	rm -rf -- "$verify_tmp"
}
trap cleanup EXIT HUP INT TERM

printf '%s\n' "$archives" | while IFS= read -r archive; do
	[ -n "$archive" ] || continue
	asset=$dist_dir/$archive
	sidecar=$asset.sha256
	[ -f "$asset" ] || fail "missing release asset: $archive"
	[ -f "$sidecar" ] || fail "missing checksum sidecar: $archive.sha256"

	manifest_lines=$(sed -n '$=' "$sidecar")
	[ "$manifest_lines" = "1" ] || fail "$archive.sha256 must contain exactly one line"
	read -r digest manifest_name extra < "$sidecar" || fail "cannot read $archive.sha256"
	[ -z "${extra:-}" ] || fail "$archive.sha256 contains unexpected fields"
	printf '%s\n' "$digest" | grep -Eq '^[0-9a-f]{64}$' || fail "$archive.sha256 has an invalid SHA-256"
	[ "$manifest_name" = "$archive" ] || \
		fail "$archive.sha256 names $manifest_name instead of $archive"
	(cd "$dist_dir" && shasum -a 256 -c "$archive.sha256") >/dev/null || \
		fail "checksum verification failed for $archive"

	case "$archive" in
	*.tar.gz)
		binary=${archive%.tar.gz}
		listing=$(tar -tzf "$asset") || fail "cannot list $archive"
		[ "$listing" = "$binary" ] || fail "$archive must contain only $binary at its root"
		extract_dir=$verify_tmp/$binary
		mkdir "$extract_dir"
		tar -xzf "$asset" -C "$extract_dir" || fail "cannot extract $archive"
		[ -f "$extract_dir/$binary" ] && [ ! -L "$extract_dir/$binary" ] || \
			fail "$archive does not contain a regular $binary"
		[ -x "$extract_dir/$binary" ] || fail "$binary is not executable"
		;;
	*.zip)
		binary=${archive%.zip}
		listing=$(unzip -Z1 "$asset") || fail "cannot list $archive"
		[ "$listing" = "metis.exe" ] || fail "$archive must contain only metis.exe at its root"
		extract_dir=$verify_tmp/$binary
		mkdir "$extract_dir"
		unzip -qq "$asset" -d "$extract_dir" || fail "cannot extract $archive"
		[ -f "$extract_dir/metis.exe" ] && [ ! -L "$extract_dir/metis.exe" ] || \
			fail "$archive does not contain a regular metis.exe"
		;;
	*)
		fail "unsupported archive: $archive"
		;;
	esac
done

case "$(uname -s):$(uname -m)" in
Linux:x86_64 | Linux:amd64)
	host_archive=metis-linux-amd64.tar.gz
	host_binary=metis-linux-amd64
	;;
Linux:aarch64 | Linux:arm64)
	host_archive=metis-linux-arm64.tar.gz
	host_binary=metis-linux-arm64
	;;
Darwin:x86_64 | Darwin:amd64)
	host_archive=metis-darwin-amd64.tar.gz
	host_binary=metis-darwin-amd64
	;;
Darwin:arm64 | Darwin:aarch64)
	host_archive=metis-darwin-arm64.tar.gz
	host_binary=metis-darwin-arm64
	;;
*)
	fail "cannot run a host smoke test on $(uname -s)/$(uname -m)"
	;;
esac

host_dir=$verify_tmp/host
mkdir "$host_dir"
tar -xzf "$dist_dir/$host_archive" -C "$host_dir"
reported_tag=$(
	"$host_dir/$host_binary" version | sed -n '1{s/[[:space:]].*$//;p;}'
) || fail "$host_binary version command failed"
[ "$reported_tag" = "$expected_tag" ] || \
	fail "$host_binary reports $reported_tag instead of $expected_tag"

printf 'verify-dist: 12 assets, checksums, archive shapes, and host binary %s verified\n' "$expected_tag"
