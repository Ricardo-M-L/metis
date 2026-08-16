#!/usr/bin/env bash
set -euo pipefail

die() {
    printf 'error: %s\n' "$*" >&2
    exit 1
}

if [ "$#" -ne 2 ]; then
    die "usage: install-local-build.sh SOURCE DESTINATION"
fi

source_path="$1"
destination="$2"

[ -f "$source_path" ] && [ ! -L "$source_path" ] && [ -x "$source_path" ] \
    || die "source is not a regular executable: ${source_path}"

destination_dir="$(dirname "$destination")"
destination_name="$(basename "$destination")"
mkdir -p "$destination_dir"
destination_dir="$(cd "$destination_dir" && pwd -P)"
destination="${destination_dir}/${destination_name}"

# The native curl installer owns <prefix>/bin/metis plus the immutable
# versions under <prefix>/share/metis/versions. A local source install must
# never replace that launcher: doing so breaks atomic updates, while also
# writing a second copy to GOBIN lets PATH keep selecting a stale binary.
managed_versions="$(dirname "$destination_dir")/share/metis/versions"
if [ -d "$managed_versions" ]; then
    die "refusing to replace curl-managed Metis at ${destination}; run './bin/metis' for this source build"
fi
if [ -L "$destination" ]; then
    die "refusing to replace symlink at ${destination}"
fi

install -m 0755 "$source_path" "$destination"
printf 'installed local build: %s\n' "$destination"
