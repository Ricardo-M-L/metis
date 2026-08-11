#!/usr/bin/env bash
# metis installer — curl one-liner front-end for the public release.
#
# Usage:
#
#   curl -fsSL \
#     https://raw.githubusercontent.com/Ricardo-M-L/metis/main/install/install.sh \
#     | bash
#
# Or after cloning:
#   bash install/install.sh
#
# Env (all optional):
#   METIS_GITHUB_TOKEN   GitHub PAT for a higher API rate limit
#                        (GITHUB_TOKEN is also accepted as fallback)
#   METIS_VERSION        Tag to install (default: latest release)
#   METIS_INSTALL_DIR    Install destination (default: $HOME/.local/bin)
#   METIS_REPO           Override repo (default: Ricardo-M-L/metis)
#   METIS_GITHUB_API_BASE  Override GitHub API base (advanced)
#   METIS_GITHUB_WEB_BASE  Override GitHub web base (advanced)

set -euo pipefail

readonly METIS_REPO="${METIS_REPO:-Ricardo-M-L/metis}"
readonly METIS_VERSION="${METIS_VERSION:-latest}"
readonly METIS_INSTALL_DIR="${METIS_INSTALL_DIR:-$HOME/.local/bin}"
readonly METIS_GITHUB_API_BASE="${METIS_GITHUB_API_BASE:-https://api.github.com}"
readonly METIS_GITHUB_WEB_BASE="${METIS_GITHUB_WEB_BASE:-https://github.com}"

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

# Fetch release metadata. Anonymous installs resolve "latest" through the
# public web redirect instead, avoiding GitHub REST's low shared-IP limit.
fetch_release_json() {
    local tag="$1" url
    if [ "$tag" = "latest" ]; then
        url="${METIS_GITHUB_API_BASE}/repos/${METIS_REPO}/releases/latest"
    else
        url="${METIS_GITHUB_API_BASE}/repos/${METIS_REPO}/releases/tags/${tag}"
    fi
    if [ -n "$TOKEN" ]; then
        curl -fsSL \
            -H "Authorization: Bearer ${TOKEN}" \
            -H "Accept: application/vnd.github+json" \
            -H "X-GitHub-Api-Version: 2022-11-28" \
            "$url"
    else
        curl -fsSL \
            -H "Accept: application/vnd.github+json" \
            -H "X-GitHub-Api-Version: 2022-11-28" \
            "$url"
    fi
}

release_tag_from_json() {
    local json="$1"
    if command -v jq >/dev/null 2>&1; then
        printf '%s' "$json" | jq -r '.tag_name'
    else
        printf '%s' "$json" \
            | tr ',' '\n' \
            | grep -E '"tag_name"' \
            | head -n1 \
            | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/'
    fi
}

resolve_release_tag() {
    local requested="$1"
    if [ "$requested" != "latest" ]; then
        printf '%s' "$requested"
        return
    fi

    if [ -n "$TOKEN" ]; then
        local json
        json="$(fetch_release_json latest)"
        release_tag_from_json "$json"
        return
    fi

    local latest_url resolved
    latest_url="$(curl -fsSL -o /dev/null -w '%{url_effective}' \
        "${METIS_GITHUB_WEB_BASE}/${METIS_REPO}/releases/latest")" \
        || err "could not resolve the latest public release"
    resolved="${latest_url##*/}"
    resolved="${resolved%%\?*}"
    resolved="${resolved%%#*}"
    [ -n "$resolved" ] && [ "$resolved" != "latest" ] \
        || err "could not resolve the latest public release tag"
    printf '%s' "$resolved"
}

download_release_file() {
    local tag="$1" name="$2" out="$3"
    local url="${METIS_GITHUB_WEB_BASE}/${METIS_REPO}/releases/download/${tag}/${name}"
    if [ -n "$TOKEN" ]; then
        curl -fsSL -H "Authorization: Bearer ${TOKEN}" "$url" -o "$out" \
            || err "could not download ${name} from release ${tag}"
    else
        curl -fsSL "$url" -o "$out" \
            || err "could not download ${name} from release ${tag}"
    fi
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

    local target tmpdir
    target="$(detect_target)"
    tmpdir="$(mktemp -d -t metis-install.XXXXXX)"
    # shellcheck disable=SC2064
    trap "rm -rf '$tmpdir'" EXIT

    log "resolving release (${METIS_VERSION})"
    local resolved_tag
    resolved_tag="$(resolve_release_tag "$METIS_VERSION")"
    [ -n "$resolved_tag" ] && [ "$resolved_tag" != "null" ] \
        || err "could not resolve release tag"

    local artifact="metis-${target}.tar.gz"
    local sumfile="${artifact}.sha256"

    log "installing metis ${resolved_tag} for ${target}"
    download_release_file "$resolved_tag" "$artifact" "${tmpdir}/${artifact}"
    download_release_file "$resolved_tag" "$sumfile"  "${tmpdir}/${sumfile}"
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

    if [ -z "$TOKEN" ]; then
        cat <<'EOF'

No GitHub token is required for public releases. If GitHub rate limits become
a problem, set METIS_GITHUB_TOKEN and run the installer again.

EOF
    fi
}

main "$@"
