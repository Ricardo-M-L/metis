#!/usr/bin/env bash
# metis installer — curl one-liner front-end for the PRIVATE release.
#
# Usage (curl one-liner against GitHub Contents API — the API path is
# more reliable than raw.githubusercontent.com for private repos; we
# saw the raw host time out where /repos/.../contents/<path> works):
#
#   export METIS_GITHUB_TOKEN=ghp_xxx           # PAT, Contents:read on metis
#   curl -fsSL \
#       -H "Authorization: Bearer $METIS_GITHUB_TOKEN" \
#       -H "Accept: application/vnd.github.raw" \
#       https://api.github.com/repos/Ricardo-M-L/metis/contents/install/install.sh \
#     | bash
#
# Or after cloning:
#   METIS_GITHUB_TOKEN=ghp_xxx bash install/install.sh
#
# Env (all optional except the token):
#   METIS_GITHUB_TOKEN   GitHub PAT with read access to Ricardo-M-L/metis
#                        (GITHUB_TOKEN is also accepted as fallback)
#   METIS_VERSION        Tag to install (default: latest release)
#   METIS_INSTALL_DIR    Install destination (default: $HOME/.local/bin)
#   METIS_REPO           Override repo (default: Ricardo-M-L/metis)

set -euo pipefail

readonly METIS_REPO="${METIS_REPO:-Ricardo-M-L/metis}"
readonly METIS_VERSION="${METIS_VERSION:-latest}"
readonly METIS_INSTALL_DIR="${METIS_INSTALL_DIR:-$HOME/.local/bin}"

# Token: prefer dedicated env var, fall back to GITHUB_TOKEN.
TOKEN="${METIS_GITHUB_TOKEN:-${GITHUB_TOKEN:-}}"

err()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }
log()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mwarn:\033[0m %s\n' "$*" >&2; }

require_cmd() {
    for c in "$@"; do
        command -v "$c" >/dev/null 2>&1 || err "missing required command: $c"
    done
}

detect_target() {
    local os arch
    case "$(uname -s)" in
        Darwin) os=darwin ;;
        Linux)  os=linux ;;
        *) err "unsupported OS: $(uname -s)" ;;
    esac
    case "$(uname -m)" in
        arm64|aarch64) arch=arm64 ;;
        x86_64|amd64)  arch=amd64 ;;
        *) err "unsupported arch: $(uname -m)" ;;
    esac
    printf '%s-%s' "$os" "$arch"
}

# Resolve "latest" or "vX.Y.Z" → release JSON.
fetch_release_json() {
    local tag="$1" url
    if [ "$tag" = "latest" ]; then
        url="https://api.github.com/repos/${METIS_REPO}/releases/latest"
    else
        url="https://api.github.com/repos/${METIS_REPO}/releases/tags/${tag}"
    fi
    curl -fsSL \
        -H "Authorization: Bearer ${TOKEN}" \
        -H "Accept: application/vnd.github+json" \
        -H "X-GitHub-Api-Version: 2022-11-28" \
        "$url"
}

# Extract a release-asset ID by name (jq optional, fall back to grep/sed).
asset_id_for() {
    local json="$1" name="$2"
    if command -v jq >/dev/null 2>&1; then
        printf '%s' "$json" | jq -r --arg n "$name" \
            '.assets[] | select(.name == $n) | .id'
    else
        # Naive but adequate: assets are flat objects with id+name fields.
        printf '%s' "$json" | tr ',' '\n' \
            | grep -E '"(id|name)"\s*:' \
            | paste - - \
            | awk -F'"' -v n="$name" '$8 == n { print $4 }'
    fi
}

download_asset() {
    local id="$1" out="$2"
    curl -fsSL \
        -H "Authorization: Bearer ${TOKEN}" \
        -H "Accept: application/octet-stream" \
        -H "X-GitHub-Api-Version: 2022-11-28" \
        "https://api.github.com/repos/${METIS_REPO}/releases/assets/${id}" \
        -o "$out"
}

verify_sha256() {
    local file="$1" sum_file="$2"
    if command -v shasum >/dev/null 2>&1; then
        (cd "$(dirname "$file")" && shasum -a 256 -c "$(basename "$sum_file")")
    elif command -v sha256sum >/dev/null 2>&1; then
        (cd "$(dirname "$file")" && sha256sum -c "$(basename "$sum_file")")
    else
        err "need shasum or sha256sum to verify download"
    fi
}

main() {
    require_cmd curl tar uname

    if [ -z "$TOKEN" ]; then
        err "$(cat <<'EOF'

This is a PRIVATE repository — anonymous downloads are not possible.
Set a GitHub Personal Access Token first:

    export METIS_GITHUB_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxx

The token only needs read access to Ricardo-M-L/metis. A fine-grained
PAT scoped to "Contents: Read-only" on this single repo is enough.

EOF
)"
    fi

    local target tmpdir
    target="$(detect_target)"
    tmpdir="$(mktemp -d -t metis-install.XXXXXX)"
    # shellcheck disable=SC2064
    trap "rm -rf '$tmpdir'" EXIT

    log "fetching release info (${METIS_VERSION})"
    local json
    json="$(fetch_release_json "$METIS_VERSION")"

    local resolved_tag
    if command -v jq >/dev/null 2>&1; then
        resolved_tag="$(printf '%s' "$json" | jq -r '.tag_name')"
    else
        resolved_tag="$(printf '%s' "$json" \
            | tr ',' '\n' \
            | grep -E '"tag_name"' \
            | head -n1 \
            | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"
    fi
    [ -n "$resolved_tag" ] || err "could not resolve release tag"

    local artifact="metis-${target}.tar.gz"
    local sumfile="${artifact}.sha256"

    local artifact_id sum_id
    artifact_id="$(asset_id_for "$json" "$artifact")"
    sum_id="$(asset_id_for "$json" "$sumfile")"
    [ -n "$artifact_id" ] || err "release ${resolved_tag} has no asset named ${artifact} — has the build workflow finished?"
    [ -n "$sum_id" ]      || err "release ${resolved_tag} has no asset named ${sumfile}"

    log "installing metis ${resolved_tag} for ${target}"
    download_asset "$artifact_id" "${tmpdir}/${artifact}"
    download_asset "$sum_id"      "${tmpdir}/${sumfile}"
    verify_sha256 "${tmpdir}/${artifact}" "${tmpdir}/${sumfile}"

    tar -xzf "${tmpdir}/${artifact}" -C "${tmpdir}"
    mkdir -p "${METIS_INSTALL_DIR}"
    install -m 0755 "${tmpdir}/metis-${target}" "${METIS_INSTALL_DIR}/metis"

    log "installed: ${METIS_INSTALL_DIR}/metis"
    if ! printf '%s' ":$PATH:" | grep -q ":${METIS_INSTALL_DIR}:"; then
        printf '\nNote: %s is not on your PATH. Add this to your shell rc:\n\n  export PATH="%s:$PATH"\n\n' \
            "${METIS_INSTALL_DIR}" "${METIS_INSTALL_DIR}"
    fi
    "${METIS_INSTALL_DIR}/metis" version || true

    cat <<EOF

Tip: keep the token in your shell rc so \`metis update\` can self-upgrade:

    export METIS_GITHUB_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxx

EOF
}

main "$@"
