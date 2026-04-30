#!/usr/bin/env bash
# metis installer — curl one-liner front-end
#
#   curl -fsSL https://example.invalid/install.sh | bash
#
# This script is LOCAL-ONLY. It expects the binary tarballs and SHA256 files
# to live under METIS_RELEASE_BASE (default: file://$HOME/.metis-releases).
# Nothing is fetched from a public registry.
#
# Override via env:
#   METIS_RELEASE_BASE   base URL or file:// path serving release artifacts
#   METIS_VERSION        version tag to install (default: latest)
#   METIS_INSTALL_DIR    install destination (default: $HOME/.local/bin)

set -euo pipefail

readonly METIS_RELEASE_BASE="${METIS_RELEASE_BASE:-file://$HOME/.metis-releases}"
readonly METIS_VERSION="${METIS_VERSION:-latest}"
readonly METIS_INSTALL_DIR="${METIS_INSTALL_DIR:-$HOME/.local/bin}"

err() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }
log() { printf '\033[36m==>\033[0m %s\n' "$*"; }

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

fetch() {
    local url="$1" out="$2"
    if [[ "$url" == file://* ]]; then
        cp "${url#file://}" "$out"
    elif command -v curl >/dev/null 2>&1; then
        curl -fsSL "$url" -o "$out"
    elif command -v wget >/dev/null 2>&1; then
        wget -q "$url" -O "$out"
    else
        err "need curl or wget"
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
    local target tmpdir
    target="$(detect_target)"
    tmpdir="$(mktemp -d -t metis-install.XXXXXX)"
    # shellcheck disable=SC2064
    trap "rm -rf '$tmpdir'" EXIT

    local artifact="metis-${target}.tar.gz"
    local base="${METIS_RELEASE_BASE%/}/${METIS_VERSION}"

    log "installing metis ${METIS_VERSION} for ${target}"
    log "source: ${base}"
    fetch "${base}/${artifact}"          "${tmpdir}/${artifact}"
    fetch "${base}/${artifact}.sha256"   "${tmpdir}/${artifact}.sha256"
    verify_sha256 "${tmpdir}/${artifact}" "${tmpdir}/${artifact}.sha256"

    tar -xzf "${tmpdir}/${artifact}" -C "${tmpdir}"
    mkdir -p "${METIS_INSTALL_DIR}"
    install -m 0755 "${tmpdir}/metis-${target}" "${METIS_INSTALL_DIR}/metis"

    log "installed: ${METIS_INSTALL_DIR}/metis"
    if ! printf '%s' ":$PATH:" | grep -q ":${METIS_INSTALL_DIR}:"; then
        printf '\nNote: %s is not on your PATH. Add this to your shell rc:\n\n  export PATH="%s:$PATH"\n\n' \
            "${METIS_INSTALL_DIR}" "${METIS_INSTALL_DIR}"
    fi
    "${METIS_INSTALL_DIR}/metis" version || true
}

main "$@"
