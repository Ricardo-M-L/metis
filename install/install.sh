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
#   METIS_INSTALL_DIR    Stable launcher directory (default: $HOME/.local/bin)
#   METIS_REPO           Override repo (default: Ricardo-M-L/metis)
#   METIS_GITHUB_API_BASE  Override GitHub API base (advanced)
#   METIS_GITHUB_WEB_BASE  Override GitHub web base (advanced)

set -euo pipefail

readonly METIS_REPO="${METIS_REPO:-Ricardo-M-L/metis}"
readonly METIS_VERSION="${METIS_VERSION:-latest}"
readonly METIS_INSTALL_DIR="${METIS_INSTALL_DIR:-$HOME/.local/bin}"
readonly METIS_GITHUB_API_BASE="${METIS_GITHUB_API_BASE:-https://api.github.com}"
readonly METIS_GITHUB_WEB_BASE="${METIS_GITHUB_WEB_BASE:-https://github.com}"
readonly METIS_HARD_MAX_BYTES=$((128 * 1024 * 1024))
readonly METIS_MAX_ARCHIVE_BYTES="${METIS_MAX_ARCHIVE_BYTES:-$METIS_HARD_MAX_BYTES}"
readonly METIS_MAX_EXPANDED_BYTES="${METIS_MAX_EXPANDED_BYTES:-$METIS_HARD_MAX_BYTES}"
readonly METIS_MAX_CHECKSUM_BYTES=$((64 * 1024))

# Token: prefer dedicated env var, fall back to GITHUB_TOKEN.
TOKEN="${METIS_GITHUB_TOKEN:-${GITHUB_TOKEN:-}}"

err()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }
log()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mwarn:\033[0m %s\n' "$*" >&2; }

TMPDIR_PATH=""
STAGING_DIR=""
TEMP_LINK=""
INSTALL_LOCK_DIR=""
INSTALL_LOCK_NONCE=""
PENDING_LOCK_DIR=""
RECLAIM_GUARD_DIR=""
RECLAIM_GUARD_NONCE=""
RECLAIM_GUARD_CREATED_AT=""
RECLAIM_GUARD_TARGET_PID=""
RECLAIM_GUARD_TARGET_NONCE=""
RECLAIM_GUARD_PENDING_DIR=""
SHARE_ROOT_PATH=""
MANAGED_ROOT_PATH=""
VERSIONS_ROOT_PATH=""
STAGING_ROOT_PATH=""
LOCKS_ROOT_PATH=""
RUNNING_ROOT_PATH=""

cleanup_on_exit() {
    local status=$?
    if [ -n "$TEMP_LINK" ] && [ -L "$TEMP_LINK" ]; then
        rm -f -- "$TEMP_LINK"
    fi
    if [ -n "$STAGING_DIR" ] && [ -n "$STAGING_ROOT_PATH" ] \
        && managed_roots_are_safe \
        && [ -d "$STAGING_DIR" ] && [ ! -L "$STAGING_DIR" ]; then
        case "$STAGING_DIR" in
            "$STAGING_ROOT_PATH"/*) rm -rf -- "$STAGING_DIR" ;;
        esac
    fi
    if [ -n "$TMPDIR_PATH" ] && [ -d "$TMPDIR_PATH" ] && [ ! -L "$TMPDIR_PATH" ]; then
        case "$TMPDIR_PATH" in
            "${TMPDIR:-/tmp}"/*|/private/var/folders/*|/var/folders/*) rm -rf -- "$TMPDIR_PATH" ;;
        esac
    fi
    if [ -n "$INSTALL_LOCK_DIR" ] && [ -n "$INSTALL_LOCK_NONCE" ] \
        && managed_roots_are_safe; then
        release_install_lock
    fi
    if [ -n "$PENDING_LOCK_DIR" ] && [ -n "$INSTALL_LOCK_NONCE" ] \
        && managed_roots_are_safe; then
        remove_owned_lock_directory "$PENDING_LOCK_DIR" "$$" "$INSTALL_LOCK_NONCE"
    fi
    if [ -n "$RECLAIM_GUARD_DIR" ] && [ -n "$RECLAIM_GUARD_NONCE" ] \
        && managed_roots_are_safe; then
        release_reclaim_guard
    fi
    if [ -n "$RECLAIM_GUARD_PENDING_DIR" ] && [ -n "$RECLAIM_GUARD_NONCE" ] \
        && managed_roots_are_safe; then
        remove_owned_reclaim_guard_directory "$RECLAIM_GUARD_PENDING_DIR" \
            "$$" "$RECLAIM_GUARD_NONCE" "$RECLAIM_GUARD_TARGET_PID" \
            "$RECLAIM_GUARD_TARGET_NONCE" "$RECLAIM_GUARD_CREATED_AT"
    fi
    return "$status"
}

direct_directory_is_safe() {
    local path="$1"
    [ -d "$path" ] && [ ! -L "$path" ]
}

ensure_direct_child_directory() {
    local parent="$1" path="$2" name
    require_direct_directory "$parent"
    [ "$(dirname "$path")" = "$parent" ] \
        || err "managed directory is not a direct child of ${parent}: ${path}"
    name="$(basename "$path")"
    if ! (
        cd -P -- "$parent" || exit 1
        if [ ! -e "$name" ] && [ ! -L "$name" ]; then
            mkdir -- "$name" 2>/dev/null || true
        fi
        [ -d "$name" ] && [ ! -L "$name" ]
    ); then
        err "refusing to use symlink or non-directory as managed directory: ${path}"
    fi
    require_direct_directory "$parent"
    require_direct_directory "$path"
}

require_direct_directory() {
    local path="$1"
    direct_directory_is_safe "$path" \
        || err "refusing to use symlink or non-directory as managed directory: ${path}"
}

validate_size_limit() {
    local name="$1" value="$2"
    [[ "$value" =~ ^[0-9]+$ ]] && (( ${#value} <= ${#METIS_HARD_MAX_BYTES} )) \
        && (( value > 0 && value <= METIS_HARD_MAX_BYTES )) \
        || err "${name} must be between 1 and ${METIS_HARD_MAX_BYTES} bytes"
}

validate_managed_roots() {
    require_direct_directory "$SHARE_ROOT_PATH"
    require_direct_directory "$MANAGED_ROOT_PATH"
    require_direct_directory "$VERSIONS_ROOT_PATH"
    require_direct_directory "$STAGING_ROOT_PATH"
    require_direct_directory "$LOCKS_ROOT_PATH"
    require_direct_directory "$RUNNING_ROOT_PATH"
}

managed_roots_are_safe() {
    [ -n "$SHARE_ROOT_PATH" ] && direct_directory_is_safe "$SHARE_ROOT_PATH" \
        && [ -n "$MANAGED_ROOT_PATH" ] && direct_directory_is_safe "$MANAGED_ROOT_PATH" \
        && [ -n "$VERSIONS_ROOT_PATH" ] && direct_directory_is_safe "$VERSIONS_ROOT_PATH" \
        && [ -n "$STAGING_ROOT_PATH" ] && direct_directory_is_safe "$STAGING_ROOT_PATH" \
        && [ -n "$LOCKS_ROOT_PATH" ] && direct_directory_is_safe "$LOCKS_ROOT_PATH" \
        && [ -n "$RUNNING_ROOT_PATH" ] && direct_directory_is_safe "$RUNNING_ROOT_PATH"
}

file_size() {
    if stat -f '%z' "$1" >/dev/null 2>&1; then
        stat -f '%z' "$1"
    else
        stat -c '%s' "$1"
    fi
}

run_bounded_pipe() {
    # Usage: run_bounded_pipe <limit> <output> <producer command...>
    local limit="$1" out="$2"
    shift 2
    local statuses
    set +e
    "$@" | head -c "$((limit + 1))" >"$out"
    statuses=("${PIPESTATUS[@]}")
    set -e

    local size
    size="$(file_size "$out")" || err "could not determine downloaded file size"
    if (( size > limit )); then
        rm -f -- "$out"
        err "download exceeds ${limit} bytes"
    fi
    if [ "${statuses[0]:-1}" -ne 0 ] || [ "${statuses[1]:-1}" -ne 0 ]; then
        rm -f -- "$out"
        return 1
    fi
    return 0
}

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
            --connect-timeout 10 --max-time 30 \
            -H "Authorization: Bearer ${TOKEN}" \
            -H "Accept: application/vnd.github+json" \
            -H "X-GitHub-Api-Version: 2022-11-28" \
            "$url"
    else
        curl -fsSL \
            --connect-timeout 10 --max-time 30 \
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
        --connect-timeout 10 --max-time 30 \
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
    local limit="$METIS_MAX_ARCHIVE_BYTES"
    case "$name" in
        *.sha256) limit="$METIS_MAX_CHECKSUM_BYTES" ;;
    esac
    if [ -n "$TOKEN" ]; then
        run_bounded_pipe "$limit" "$out" curl -fsSL \
            --connect-timeout 10 --max-time 300 \
            -H "Authorization: Bearer ${TOKEN}" "$url" \
            || err "could not download ${name} from release ${tag}"
    else
        run_bounded_pipe "$limit" "$out" curl -fsSL \
            --connect-timeout 10 --max-time 300 "$url" \
            || err "could not download ${name} from release ${tag}"
    fi
}

extract_release_binary_bounded() {
    local archive="$1" member="$2" out="$3"
    local statuses size
    set +e
    tar -xOzf "$archive" "$member" \
        | head -c "$((METIS_MAX_EXPANDED_BYTES + 1))" >"$out"
    statuses=("${PIPESTATUS[@]}")
    set -e

    size="$(file_size "$out")" || err "could not determine extracted binary size"
    if (( size > METIS_MAX_EXPANDED_BYTES )); then
        rm -f -- "$out"
        err "expanded binary exceeds ${METIS_MAX_EXPANDED_BYTES} bytes"
    fi
    if [ "${statuses[0]:-1}" -ne 0 ] || [ "${statuses[1]:-1}" -ne 0 ] || (( size == 0 )); then
        rm -f -- "$out"
        err "release archive does not contain a valid ${member} binary"
    fi
    chmod 0755 "$out"
}

verify_sha256() {
    local file="$1" sum_file="$2"
    local name expected actual
    name="$(basename "$file")"
    expected="$(awk -v name="$name" '$2 == name || $2 == "*" name { print $1; exit }' "$sum_file")"
    [[ "$expected" =~ ^[[:xdigit:]]{64}$ ]] \
        || err "release checksum does not contain a valid SHA256 for ${name}"
    if command -v shasum >/dev/null 2>&1; then
        actual="$(shasum -a 256 "$file" | awk '{ print $1 }')"
    elif command -v sha256sum >/dev/null 2>&1; then
        actual="$(sha256sum "$file" | awk '{ print $1 }')"
    else
        err "need shasum or sha256sum to verify download"
    fi
    actual="$(printf '%s' "$actual" | tr '[:upper:]' '[:lower:]')"
    expected="$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]')"
    [ "$actual" = "$expected" ] \
        || err "SHA256 mismatch for ${name}"
}

sha256_digest() {
    local file="$1"
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$file" | awk '{ print $1 }'
    elif command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$file" | awk '{ print $1 }'
    else
        err "need shasum or sha256sum to verify download"
    fi
}

validate_version_name() {
    local version="$1"
    [[ "$version" =~ ^[A-Za-z0-9][A-Za-z0-9.+_-]*$ ]] \
        && (( ${#version} <= 128 )) \
        && [ "$version" != "." ] \
        && [ "$version" != ".." ] \
        || err "release tag cannot be used as a local version name: $version"
}

path_mtime() {
    if stat -f '%m' "$1" >/dev/null 2>&1; then
        stat -f '%m' "$1"
    else
        stat -c '%Y' "$1"
    fi
}

process_is_alive_or_unknown() {
    local pid="$1"
    kill -0 "$pid" >/dev/null 2>&1 && return 0
    # A failed kill can mean either ESRCH or EPERM. ps gives us a second,
    # read-only check; disagreement is treated as "possibly alive".
    [ -n "$(ps -p "$pid" -o pid= 2>/dev/null)" ] && return 0
    return 1
}

valid_install_lock_nonce() {
    local nonce="$1"
    [[ "$nonce" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] \
        && (( ${#nonce} <= 128 )) \
        && [ "$nonce" != "." ] && [ "$nonce" != ".." ]
}

new_install_lock_nonce() {
    printf '%s-%s-%s-%s' "$$" "$RANDOM" "$RANDOM" "$(date +%s)"
}

read_install_lock_owner() {
    local lock_dir="$1" owner
    local pid nonce created_at size
    owner="${lock_dir}/owner.json"
    [ -d "$lock_dir" ] && [ ! -L "$lock_dir" ] \
        && [ -f "$owner" ] && [ ! -L "$owner" ] || return 1
    size="$(file_size "$owner")" || return 1
    (( size > 0 && size <= METIS_MAX_CHECKSUM_BYTES )) || return 1
    pid="$(sed -n 's/.*"pid"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$owner" | head -n1)"
    nonce="$(sed -n 's/.*"nonce"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$owner" | head -n1)"
    created_at="$(sed -n 's/.*"created_at"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$owner" | head -n1)"
    [[ "$pid" =~ ^[0-9]+$ ]] && (( pid > 0 )) \
        && valid_install_lock_nonce "$nonce" \
        && [[ "$created_at" =~ ^[0-9]+$ ]] && (( created_at > 0 )) \
        || return 1
    printf '%s\t%s\t%s\n' "$pid" "$nonce" "$created_at"
}

install_lock_owner_matches() {
    local lock_dir="$1" want_pid="$2" want_nonce="$3" want_created_at="${4:-}"
    local record pid nonce created_at
    record="$(read_install_lock_owner "$lock_dir")" || return 1
    IFS=$'\t' read -r pid nonce created_at <<<"$record"
    [ "$pid" = "$want_pid" ] && [ "$nonce" = "$want_nonce" ] \
        && { [ -z "$want_created_at" ] || [ "$created_at" = "$want_created_at" ]; }
}

remove_owned_lock_directory() {
    local path="$1" pid="$2" nonce="$3"
    [ -d "$path" ] && [ ! -L "$path" ] || return 0
    install_lock_owner_matches "$path" "$pid" "$nonce" || return 0
    rm -rf -- "$path"
}

# BSD/GNU mv treat an existing directory as a container. A sentinel whose
# name equals the source directory prevents a losing no-clobber move from
# nesting a successor inside an already-published quarantine/tombstone.
ensure_lock_move_sentinel() {
    local dir="$1" sentinel
    [ -d "$dir" ] && [ ! -L "$dir" ] || return 1
    sentinel="${dir}/$(basename "$dir")"
    if [ ! -e "$sentinel" ] && [ ! -L "$sentinel" ]; then
        (set -C; umask 077; : >"$sentinel") 2>/dev/null || true
    fi
    [ -f "$sentinel" ] && [ ! -L "$sentinel" ] || return 1
}

read_reclaim_guard_owner() {
    local guard_dir="$1" owner pid nonce target_pid target_nonce created_at size
    owner="${guard_dir}/owner.json"
    [ -d "$guard_dir" ] && [ ! -L "$guard_dir" ] \
        && [ -f "$owner" ] && [ ! -L "$owner" ] || return 1
    size="$(file_size "$owner")" || return 1
    (( size > 0 && size <= METIS_MAX_CHECKSUM_BYTES )) || return 1
    pid="$(sed -n 's/.*"pid"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$owner" | head -n1)"
    nonce="$(sed -n 's/.*"nonce"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$owner" | head -n1)"
    target_pid="$(sed -n 's/.*"target_pid"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$owner" | head -n1)"
    target_nonce="$(sed -n 's/.*"target_nonce"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$owner" | head -n1)"
    created_at="$(sed -n 's/.*"created_at"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$owner" | head -n1)"
    [[ "$pid" =~ ^[0-9]+$ ]] && (( pid > 0 )) \
        && valid_install_lock_nonce "$nonce" \
        && [[ "$target_pid" =~ ^[0-9]+$ ]] && (( target_pid > 0 )) \
        && valid_install_lock_nonce "$target_nonce" \
        && [[ "$created_at" =~ ^[0-9]+$ ]] && (( created_at > 0 )) \
        || return 1
    printf '%s\t%s\t%s\t%s\t%s\n' \
        "$pid" "$nonce" "$target_pid" "$target_nonce" "$created_at"
}

reclaim_guard_owner_matches() {
    local guard_dir="$1" want_pid="$2" want_nonce="$3" want_target_pid="$4"
    local want_target_nonce="$5" want_created_at="${6:-}"
    local record pid nonce target_pid target_nonce created_at
    record="$(read_reclaim_guard_owner "$guard_dir")" || return 1
    IFS=$'\t' read -r pid nonce target_pid target_nonce created_at <<<"$record"
    [ "$pid" = "$want_pid" ] && [ "$nonce" = "$want_nonce" ] \
        && [ "$target_pid" = "$want_target_pid" ] \
        && [ "$target_nonce" = "$want_target_nonce" ] \
        && { [ -z "$want_created_at" ] || [ "$created_at" = "$want_created_at" ]; }
}

remove_owned_reclaim_guard_directory() {
    local path="$1" pid="$2" nonce="$3" target_pid="$4" target_nonce="$5" created_at="${6:-}"
    [ -d "$path" ] && [ ! -L "$path" ] || return 0
    reclaim_guard_owner_matches "$path" "$pid" "$nonce" "$target_pid" "$target_nonce" "$created_at" \
        || return 0
    rm -rf -- "$path"
}

lock_test_pause() {
    local phase="$1" hook_dir="${METIS_INSTALLER_TEST_HOOK_DIR:-}"
    [ -n "$hook_dir" ] || return 0
    [ -d "$hook_dir" ] && [ ! -L "$hook_dir" ] || err "invalid installer test hook directory"
    [ -f "${hook_dir}/${phase}.pause" ] && [ ! -L "${hook_dir}/${phase}.pause" ] || return 0
    : >"${hook_dir}/${phase}.ready"
    while [ ! -e "${hook_dir}/${phase}.continue" ]; do
        sleep 0.01
    done
}

cleanup_pending_lock_candidate() {
    local pending="$1" fixed="$2" pid="$3" nonce="$4" nested
    remove_owned_lock_directory "$pending" "$pid" "$nonce"
    if [ -d "$fixed" ] && [ ! -L "$fixed" ]; then
        nested="${fixed}/$(basename "$pending")"
        remove_owned_lock_directory "$nested" "$pid" "$nonce"
    fi
}

# Publish a fully-owned pending directory. On both BSD and GNU mv, an absent
# destination is an atomic directory rename. If a contender wins first, mv -n
# may leave the source in place or place it as a uniquely-named child of the
# winning directory; ownership verification below distinguishes every outcome
# and removes only this process's pending artifact.
publish_install_lock() {
    local locks_dir="$1" fixed="$2" nonce pending owner created_at mv_status
    nonce="$(new_install_lock_nonce)"
    pending="${locks_dir}/install.lock.d.pending.${nonce}"
    owner="${pending}/owner.json"
    created_at="$(date +%s)"

    mkdir -m 0700 -- "$pending" 2>/dev/null \
        || err "create pending update lock: ${pending}"
    INSTALL_LOCK_NONCE="$nonce"
    PENDING_LOCK_DIR="$pending"
    (umask 077; printf '{"pid":%s,"nonce":"%s","created_at":%s}\n' \
        "$$" "$nonce" "$created_at" >"$owner") \
        || err "write pending update lock owner"
    install_lock_owner_matches "$pending" "$$" "$nonce" "$created_at" \
        || err "pending update lock owner verification failed"

    lock_test_pause pending
    validate_managed_roots
    if [ -e "$fixed" ] || [ -L "$fixed" ]; then
        cleanup_pending_lock_candidate "$pending" "$fixed" "$$" "$nonce"
        PENDING_LOCK_DIR=""
        INSTALL_LOCK_NONCE=""
        return 1
    fi

    set +e
    mv -n -- "$pending" "$fixed"
    mv_status=$?
    set -e
    if [ "$mv_status" -eq 0 ] && [ -d "$fixed" ] && [ ! -L "$fixed" ] \
        && install_lock_owner_matches "$fixed" "$$" "$nonce" "$created_at"; then
        PENDING_LOCK_DIR=""
        INSTALL_LOCK_DIR="$fixed"
        INSTALL_LOCK_NONCE="$nonce"
        lock_test_pause acquired
        return 0
    fi

    cleanup_pending_lock_candidate "$pending" "$fixed" "$$" "$nonce"
    PENDING_LOCK_DIR=""
    INSTALL_LOCK_NONCE=""
    if [ -e "$fixed" ] || [ -L "$fixed" ]; then
        return 1
    fi
    err "could not atomically publish update lock"
}

move_owned_lock_to_quarantine() {
    local source="$1" destination="$2" pid="$3" nonce="$4" created_at="$5"
    local mv_status nested
    [ ! -e "$destination" ] && [ ! -L "$destination" ] || return 1
    install_lock_owner_matches "$source" "$pid" "$nonce" "$created_at" || return 1
    ensure_lock_move_sentinel "$source" || return 1

    set +e
    mv -n -- "$source" "$destination"
    mv_status=$?
    set -e
    [ "$mv_status" -eq 0 ] || return 1
    if [ ! -e "$source" ] && [ ! -L "$source" ] \
        && install_lock_owner_matches "$destination" "$pid" "$nonce" "$created_at"; then
        printf '%s\n' "$destination"
        return 0
    fi
    nested="${destination}/$(basename "$source")"
    if [ ! -e "$source" ] && [ ! -L "$source" ] \
        && [ -d "$destination" ] && [ ! -L "$destination" ] \
        && install_lock_owner_matches "$nested" "$pid" "$nonce" "$created_at"; then
        printf '%s\n' "$nested"
        return 0
    fi
    return 1
}

cleanup_pending_reclaim_guard_candidate() {
    local pending="$1" fixed="$2" pid="$3" nonce="$4" target_pid="$5" target_nonce="$6" created_at="$7"
    local nested
    remove_owned_reclaim_guard_directory "$pending" "$pid" "$nonce" "$target_pid" "$target_nonce" "$created_at"
    if [ -d "$fixed" ] && [ ! -L "$fixed" ]; then
        nested="${fixed}/$(basename "$pending")"
        remove_owned_reclaim_guard_directory "$nested" "$pid" "$nonce" "$target_pid" "$target_nonce" "$created_at"
    fi
}

publish_reclaim_guard() {
    local locks_dir="$1" fixed="$2" target_pid="$3" target_nonce="$4"
    local nonce pending owner created_at mv_status
    nonce="$(new_install_lock_nonce)"
    pending="${fixed}.pending.${nonce}"
    owner="${pending}/owner.json"
    created_at="$(date +%s)"

    mkdir -m 0700 -- "$pending" 2>/dev/null \
        || err "create pending reclaim guard: ${pending}"
    RECLAIM_GUARD_NONCE="$nonce"
    RECLAIM_GUARD_CREATED_AT="$created_at"
    RECLAIM_GUARD_TARGET_PID="$target_pid"
    RECLAIM_GUARD_TARGET_NONCE="$target_nonce"
    RECLAIM_GUARD_PENDING_DIR="$pending"
    (umask 077; printf '{"pid":%s,"nonce":"%s","target_pid":%s,"target_nonce":"%s","created_at":%s}\n' \
        "$$" "$nonce" "$target_pid" "$target_nonce" "$created_at" >"$owner") \
        || err "write pending reclaim guard owner"
    reclaim_guard_owner_matches "$pending" "$$" "$nonce" "$target_pid" "$target_nonce" "$created_at" \
        || err "pending reclaim guard owner verification failed"

    lock_test_pause guard-pending
    validate_managed_roots
    if [ -e "$fixed" ] || [ -L "$fixed" ]; then
        cleanup_pending_reclaim_guard_candidate \
            "$pending" "$fixed" "$$" "$nonce" "$target_pid" "$target_nonce" "$created_at"
        RECLAIM_GUARD_PENDING_DIR=""
        RECLAIM_GUARD_NONCE=""
        return 1
    fi

    set +e
    mv -n -- "$pending" "$fixed"
    mv_status=$?
    set -e
    if [ "$mv_status" -eq 0 ] && [ -d "$fixed" ] && [ ! -L "$fixed" ] \
        && reclaim_guard_owner_matches "$fixed" "$$" "$nonce" "$target_pid" "$target_nonce" "$created_at"; then
        RECLAIM_GUARD_PENDING_DIR=""
        RECLAIM_GUARD_DIR="$fixed"
        lock_test_pause guard-acquired
        return 0
    fi

    cleanup_pending_reclaim_guard_candidate \
        "$pending" "$fixed" "$$" "$nonce" "$target_pid" "$target_nonce" "$created_at"
    RECLAIM_GUARD_PENDING_DIR=""
    RECLAIM_GUARD_NONCE=""
    if [ -e "$fixed" ] || [ -L "$fixed" ]; then
        return 1
    fi
    err "could not atomically publish reclaim guard"
}

move_owned_reclaim_guard_to_quarantine() {
    local source="$1" destination="$2" pid="$3" nonce="$4" target_pid="$5" target_nonce="$6" created_at="$7"
    local mv_status nested
    [ ! -e "$destination" ] && [ ! -L "$destination" ] || return 1
    reclaim_guard_owner_matches "$source" "$pid" "$nonce" "$target_pid" "$target_nonce" "$created_at" \
        || return 1
    ensure_lock_move_sentinel "$source" || return 1
    set +e
    mv -n -- "$source" "$destination"
    mv_status=$?
    set -e
    [ "$mv_status" -eq 0 ] || return 1
    if [ ! -e "$source" ] && [ ! -L "$source" ] \
        && reclaim_guard_owner_matches "$destination" "$pid" "$nonce" "$target_pid" "$target_nonce" "$created_at"; then
        printf '%s\n' "$destination"
        return 0
    fi
    nested="${destination}/$(basename "$source")"
    if [ ! -e "$source" ] && [ ! -L "$source" ] \
        && [ -d "$destination" ] && [ ! -L "$destination" ] \
        && reclaim_guard_owner_matches "$nested" "$pid" "$nonce" "$target_pid" "$target_nonce" "$created_at"; then
        printf '%s\n' "$nested"
        return 0
    fi
    return 1
}

release_reclaim_guard() {
    local quarantine moved
    [ -n "$RECLAIM_GUARD_DIR" ] && [ -n "$RECLAIM_GUARD_NONCE" ] || return 0
    quarantine="${RECLAIM_GUARD_DIR}.release.${RECLAIM_GUARD_NONCE}"
    moved="$(move_owned_reclaim_guard_to_quarantine \
        "$RECLAIM_GUARD_DIR" "$quarantine" "$$" "$RECLAIM_GUARD_NONCE" \
        "$RECLAIM_GUARD_TARGET_PID" "$RECLAIM_GUARD_TARGET_NONCE" \
        "$RECLAIM_GUARD_CREATED_AT")" || return 0
    remove_owned_reclaim_guard_directory "$moved" "$$" "$RECLAIM_GUARD_NONCE" \
        "$RECLAIM_GUARD_TARGET_PID" "$RECLAIM_GUARD_TARGET_NONCE" \
        "$RECLAIM_GUARD_CREATED_AT"
    RECLAIM_GUARD_DIR=""
    RECLAIM_GUARD_NONCE=""
    RECLAIM_GUARD_CREATED_AT=""
    RECLAIM_GUARD_TARGET_PID=""
    RECLAIM_GUARD_TARGET_NONCE=""
}

retire_dead_reclaim_guard() {
    local fixed="$1" pid="$2" nonce="$3" target_pid="$4" target_nonce="$5" created_at="$6"
    local retired moved
    lock_test_pause guard-dead-observed
    reclaim_guard_owner_matches "$fixed" "$pid" "$nonce" "$target_pid" "$target_nonce" "$created_at" \
        || return 1
    process_is_alive_or_unknown "$pid" && return 1
    retired="${fixed}.retired.${nonce}"
    moved="$(move_owned_reclaim_guard_to_quarantine \
        "$fixed" "$retired" "$pid" "$nonce" "$target_pid" "$target_nonce" "$created_at")" \
        || return 1
    # Retired guard artifacts are permanent ABA tombstones. Never delete or
    # age-clean them: an observer paused on the old guard must be unable to
    # move a successor into the same retirement name.
    [ "$moved" = "$retired" ] || return 1
    return 0
}

acquire_reclaim_guard() {
    local locks_dir="$1" target_pid="$2" target_nonce="$3"
    local fixed record pid nonce owner_target_pid owner_target_nonce created_at
    fixed="${locks_dir}/install.reclaim.${target_nonce}.d"
    while :; do
        if [ ! -e "$fixed" ] && [ ! -L "$fixed" ]; then
            if publish_reclaim_guard "$locks_dir" "$fixed" "$target_pid" "$target_nonce"; then
                return 0
            fi
            continue
        fi
        [ -d "$fixed" ] && [ ! -L "$fixed" ] \
            || err "reclaim guard is a symlink or non-directory: ${fixed}"
        record="$(read_reclaim_guard_owner "$fixed")" \
            || err "reclaim guard has missing or invalid owner state; manual recovery required: ${fixed}"
        IFS=$'\t' read -r pid nonce owner_target_pid owner_target_nonce created_at <<<"$record"
        [ "$owner_target_pid" = "$target_pid" ] && [ "$owner_target_nonce" = "$target_nonce" ] \
            || err "reclaim guard target does not match fixed lock owner: ${fixed}"
        if process_is_alive_or_unknown "$pid"; then
            return 1
        fi
        retire_dead_reclaim_guard \
            "$fixed" "$pid" "$nonce" "$owner_target_pid" "$owner_target_nonce" "$created_at" \
            || err "dead reclaim guard could not be retired safely: ${fixed}"
    done
}

reclaim_dead_install_lock() {
    local locks_dir="$1" fixed="$2" expected_pid="$3" expected_nonce="$4" expected_created_at="$5"
    local quarantine moved nonce record pid current_nonce created_at

    lock_test_pause dead-observed
    acquire_reclaim_guard "$locks_dir" "$expected_pid" "$expected_nonce" || return 1

    record="$(read_install_lock_owner "$fixed")" || {
        release_reclaim_guard
        return 1
    }
    IFS=$'\t' read -r pid current_nonce created_at <<<"$record"
    if [ "$pid" != "$expected_pid" ] || [ "$current_nonce" != "$expected_nonce" ] \
        || [ "$created_at" != "$expected_created_at" ] \
        || process_is_alive_or_unknown "$pid"; then
        release_reclaim_guard
        return 1
    fi

    nonce="$(new_install_lock_nonce)"
    quarantine="${locks_dir}/install.lock.d.stale.${nonce}"
    moved="$(move_owned_lock_to_quarantine \
        "$fixed" "$quarantine" "$pid" "$current_nonce" "$created_at")" || {
        release_reclaim_guard
        return 1
    }
    remove_owned_lock_directory "$moved" "$pid" "$current_nonce"
    release_reclaim_guard
    return 0
}

release_install_lock() {
    local record pid nonce created_at quarantine moved
    [ -d "$INSTALL_LOCK_DIR" ] && [ ! -L "$INSTALL_LOCK_DIR" ] || return 0
    record="$(read_install_lock_owner "$INSTALL_LOCK_DIR")" || return 0
    IFS=$'\t' read -r pid nonce created_at <<<"$record"
    [ "$pid" = "$$" ] && [ "$nonce" = "$INSTALL_LOCK_NONCE" ] || return 0

    quarantine="${LOCKS_ROOT_PATH}/install.lock.d.release.${nonce}"
    moved="$(move_owned_lock_to_quarantine \
        "$INSTALL_LOCK_DIR" "$quarantine" "$pid" "$nonce" "$created_at")" || return 0
    remove_owned_lock_directory "$moved" "$pid" "$nonce"
    INSTALL_LOCK_DIR=""
    INSTALL_LOCK_NONCE=""
}

acquire_install_lock() {
    local locks_dir="$1" fixed record pid nonce created_at
    require_direct_directory "$locks_dir"
    fixed="${locks_dir}/install.lock.d"

    while :; do
        if [ ! -e "$fixed" ] && [ ! -L "$fixed" ]; then
            if publish_install_lock "$locks_dir" "$fixed"; then
                return 0
            fi
            continue
        fi
        [ -d "$fixed" ] && [ ! -L "$fixed" ] \
            || err "refusing to use symlink or non-directory as install lock: ${fixed}"
        record="$(read_install_lock_owner "$fixed")" \
            || err "fixed update lock has missing or invalid owner state; manual recovery required: ${fixed}"
        IFS=$'\t' read -r pid nonce created_at <<<"$record"
        process_is_alive_or_unknown "$pid" \
            && err "another metis install is already running (pid ${pid})"
        if reclaim_dead_install_lock "$locks_dir" "$fixed" "$pid" "$nonce" "$created_at"; then
            continue
        fi
        # The fixed owner may have changed while this contender waited for a
        # reclaim guard. Re-read it on the next iteration; never act on the old
        # snapshot.
        sleep 0.01
    done
}

remove_stale_temporary_entries() {
    local staging_root="$1" install_dir="$2" now entry modified
    validate_managed_roots
    now="$(date +%s)"

    for entry in "$staging_root"/* "$staging_root"/.[!.]* "$staging_root"/..?*; do
        [ -e "$entry" ] || [ -L "$entry" ] || continue
        modified="$(path_mtime "$entry")" || continue
        if (( now - modified >= 3600 )); then
            if [ -d "$entry" ] && [ ! -L "$entry" ]; then
                rm -rf -- "$entry"
            else
                rm -f -- "$entry"
            fi
        fi
    done
    for entry in "$install_dir"/.metis-link-*; do
        [ -L "$entry" ] || continue
        modified="$(path_mtime "$entry")" || continue
        if (( now - modified >= 3600 )); then
            rm -f -- "$entry"
        fi
    done
}

collect_running_version_locks() {
    local running_root="$1" protected_file="$2"
    local version_dir version lock pid recorded_pid recorded_version recorded_nonce recorded_exec recorded_created_at
    : >"$protected_file"
    if [ ! -e "$running_root" ] && [ ! -L "$running_root" ]; then
        return 0
    fi
    if [ ! -d "$running_root" ] || [ -L "$running_root" ]; then
        printf '%s\n' '*' >"$protected_file"
        return
    fi

    for version_dir in "$running_root"/*; do
        [ -e "$version_dir" ] || [ -L "$version_dir" ] || continue
        version="$(basename "$version_dir")"
        if [ ! -d "$version_dir" ] || [ -L "$version_dir" ] \
            || [[ ! "$version" =~ ^[A-Za-z0-9][A-Za-z0-9.+_-]*$ ]] \
            || (( ${#version} > 128 )); then
            # Unknown state under the running-lock namespace means cleanup
            # cannot prove safety. An empty marker makes the caller skip all.
            printf '%s\n' '*' >"$protected_file"
            return
        fi

        for lock in "$version_dir"/* "$version_dir"/.[!.]* "$version_dir"/..?*; do
            [ -e "$lock" ] || [ -L "$lock" ] || continue
            pid="$(basename "$lock" .json)"
            if [ "$(basename "$lock")" != "${pid}.json" ] || [[ ! "$pid" =~ ^[0-9]+$ ]] || (( pid <= 0 )); then
                printf '%s\n' '*' >"$protected_file"
                return
            fi
            if [ ! -f "$lock" ] || [ -L "$lock" ]; then
                printf '%s\n' '*' >"$protected_file"
                return
            fi
            recorded_pid="$(sed -n 's/.*"pid"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$lock" | head -n1)"
            recorded_version="$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$lock" | head -n1)"
            recorded_nonce="$(sed -n 's/.*"nonce"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$lock" | head -n1)"
            recorded_exec="$(sed -n 's/.*"exec_path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$lock" | head -n1)"
            recorded_created_at="$(sed -n 's/.*"created_at"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$lock" | head -n1)"
            if [ "$recorded_pid" != "$pid" ] || [ "$recorded_version" != "$version" ] \
                || [ -z "$recorded_nonce" ] || [ -z "$recorded_exec" ] \
                || [[ ! "$recorded_created_at" =~ ^[0-9]+$ ]]; then
                printf '%s\n' "$version" >>"$protected_file"
                continue
            fi
            if process_is_alive_or_unknown "$pid"; then
                printf '%s\n' "$version" >>"$protected_file"
            else
                rm -f -- "$lock"
            fi
        done
        rmdir "$version_dir" 2>/dev/null || true
    done
}

# Keep the current version plus the two newest rollback versions. Version
# directories are immutable and direct children of versions_dir. If another
# updater has left any lock state that this bootstrap script cannot prove is
# stale, retain everything; deleting too little is safer than deleting a binary
# that a running process may still need.
prune_old_versions() {
    local versions_dir="$1" current_bin="$2" locks_dir="$3" launcher="$4"
    local launcher_target entry name modified kept=0
    local listing protected_file running_root

    validate_managed_roots

    [ -L "$launcher" ] || {
        warn "launcher is not managed; keeping all installed versions"
        return
    }
    launcher_target="$(readlink "$launcher")" || return
    [ "$launcher_target" = "$current_bin" ] || {
        warn "launcher target is not managed; keeping all installed versions"
        return
    }

    protected_file="${TMPDIR_PATH}/protected-versions"
    running_root="${locks_dir}/running"
    collect_running_version_locks "$running_root" "$protected_file"
    if grep -Fqx '*' "$protected_file"; then
        warn "unknown running-version lock state found; keeping old versions"
        return
    fi

    listing="${TMPDIR_PATH}/versions-by-mtime"
    : >"$listing"
    for entry in "$versions_dir"/*; do
        [ -d "$entry" ] && [ ! -L "$entry" ] || continue
        name="$(basename "$entry")"
        [[ "$name" =~ ^[A-Za-z0-9][A-Za-z0-9.+_-]*$ ]] || continue
        (( ${#name} <= 128 )) || continue
        [ -f "$entry/metis" ] && [ ! -L "$entry/metis" ] || continue
        [ "$entry/metis" = "$current_bin" ] && continue
        grep -Fqx "$name" "$protected_file" && continue
        modified="$(path_mtime "$entry")" || continue
        printf '%s\t%s\n' "$modified" "$name" >>"$listing"
    done

    while IFS=$'\t' read -r _ name; do
        [ -n "$name" ] || continue
        kept=$((kept + 1))
        if (( kept > 2 )); then
            entry="${versions_dir}/${name}"
            validate_managed_roots
            [ -d "$entry" ] && [ ! -L "$entry" ] || continue
            rm -rf -- "$entry"
        fi
    done < <(sort -t $'\t' -k1,1nr -k2,2r "$listing")
}

launcher_is_safe_to_replace() {
    local launcher="$1" versions_dir="$2" target output reported_version
    [ -e "$launcher" ] || [ -L "$launcher" ] || return 0

    if [ -L "$launcher" ]; then
        target="$(readlink "$launcher")" || return 1
        case "$target" in
            "$versions_dir"/*/metis) return 0 ;;
            *) return 1 ;;
        esac
    fi

    [ -f "$launcher" ] && [ -x "$launcher" ] || return 1
    output="$("$launcher" version 2>/dev/null || true)"
    reported_version="$(printf '%s\n' "$output" | awk 'NR == 1 { print $1; exit }')"
    [[ "$reported_version" =~ ^v?[A-Za-z0-9][A-Za-z0-9.+_-]*$ ]] || return 1
    (( ${#reported_version} <= 129 )) || return 1
    case "$output" in *Metis*) return 0 ;; esac
    return 1
}

migrate_legacy_launcher() {
    local launcher="$1" versions_dir="$2" staging_root="$3"
    local output reported_version legacy_version legacy_dir legacy_bin staged_bin
    require_direct_directory "$versions_dir"
    require_direct_directory "$staging_root"
    [ -f "$launcher" ] && [ ! -L "$launcher" ] || return 0

    output="$("$launcher" version 2>/dev/null || true)"
    reported_version="$(printf '%s\n' "$output" | awk 'NR == 1 { print $1; exit }')"
    legacy_version="${reported_version#v}"
    validate_version_name "$legacy_version"

    legacy_dir="${versions_dir}/${legacy_version}"
    legacy_bin="${legacy_dir}/metis"
    STAGING_DIR="$(mktemp -d "${staging_root}/legacy-${legacy_version}.XXXXXX")"
    staged_bin="${STAGING_DIR}/metis"
    install -m 0755 "$launcher" "$staged_bin"
    touch -r "$launcher" "$staged_bin" "$STAGING_DIR"

    if [ -e "$legacy_dir" ] || [ -L "$legacy_dir" ]; then
        [ -d "$legacy_dir" ] && [ ! -L "$legacy_dir" ] \
            && [ -f "$legacy_bin" ] && [ ! -L "$legacy_bin" ] \
            || err "existing legacy version path is not managed: ${legacy_dir}"
        [ "$(sha256_digest "$staged_bin")" = "$(sha256_digest "$legacy_bin")" ] \
            || err "existing ${legacy_version} binary differs from the current launcher"
        rm -rf -- "$STAGING_DIR"
        STAGING_DIR=""
        return
    fi

    validate_managed_roots
    mv -- "$STAGING_DIR" "$legacy_dir"
    STAGING_DIR=""
}

main() {
    require_cmd awk curl date grep head install ln mv ps readlink sed sort stat tar touch tr uname
    validate_size_limit METIS_MAX_ARCHIVE_BYTES "$METIS_MAX_ARCHIVE_BYTES"
    validate_size_limit METIS_MAX_EXPANDED_BYTES "$METIS_MAX_EXPANDED_BYTES"

    local target install_dir share_dir data_dir versions_dir staging_root locks_dir running_root launcher
    local resolved_tag version artifact sumfile version_dir versioned_bin staged_bin reported_version
    local activated_output activated_version
    target="$(detect_target)"
    mkdir -p "$METIS_INSTALL_DIR"
    install_dir="$(cd "$METIS_INSTALL_DIR" && pwd -P)"
    share_dir="$(dirname "$install_dir")/share"
    data_dir="${share_dir}/metis"
    versions_dir="${data_dir}/versions"
    staging_root="${data_dir}/staging"
    locks_dir="${data_dir}/locks"
    running_root="${locks_dir}/running"
    launcher="${install_dir}/metis"

    SHARE_ROOT_PATH="$share_dir"
    MANAGED_ROOT_PATH="$data_dir"
    VERSIONS_ROOT_PATH="$versions_dir"
    STAGING_ROOT_PATH="$staging_root"
    LOCKS_ROOT_PATH="$locks_dir"
    RUNNING_ROOT_PATH="$running_root"

    ensure_direct_child_directory "$(dirname "$share_dir")" "$share_dir"
    ensure_direct_child_directory "$share_dir" "$data_dir"
    ensure_direct_child_directory "$data_dir" "$versions_dir"
    ensure_direct_child_directory "$data_dir" "$staging_root"
    ensure_direct_child_directory "$data_dir" "$locks_dir"
    ensure_direct_child_directory "$locks_dir" "$running_root"
    validate_managed_roots

    launcher_is_safe_to_replace "$launcher" "$versions_dir" \
        || err "refusing to replace unmanaged launcher: ${launcher}"

    TMPDIR_PATH="$(mktemp -d -t metis-install.XXXXXX)"
    trap cleanup_on_exit EXIT
    acquire_install_lock "$locks_dir"
    remove_stale_temporary_entries "$staging_root" "$install_dir"

    log "resolving release (${METIS_VERSION})"
    resolved_tag="$(resolve_release_tag "$METIS_VERSION")"
    [ -n "$resolved_tag" ] && [ "$resolved_tag" != "null" ] \
        || err "could not resolve release tag"

    version="${resolved_tag#v}"
    validate_version_name "$version"
    artifact="metis-${target}.tar.gz"
    sumfile="${artifact}.sha256"

    log "installing metis ${resolved_tag} for ${target}"
    download_release_file "$resolved_tag" "$artifact" "${TMPDIR_PATH}/${artifact}"
    download_release_file "$resolved_tag" "$sumfile"  "${TMPDIR_PATH}/${sumfile}"
    verify_sha256 "${TMPDIR_PATH}/${artifact}" "${TMPDIR_PATH}/${sumfile}"

    extract_release_binary_bounded \
        "${TMPDIR_PATH}/${artifact}" "metis-${target}" "${TMPDIR_PATH}/metis-${target}"

    validate_managed_roots
    STAGING_DIR="$(mktemp -d "${staging_root}/install-${version}.XXXXXX")"
    staged_bin="${STAGING_DIR}/metis"
    install -m 0755 "${TMPDIR_PATH}/metis-${target}" "$staged_bin"
    reported_version="$("$staged_bin" version 2>/dev/null | awk 'NR == 1 { print $1; exit }')" \
        || err "downloaded binary failed its version smoke test"
    [ "${reported_version#v}" = "$version" ] \
        || err "downloaded binary reports ${reported_version:-no-version}, expected ${resolved_tag}"

    version_dir="${versions_dir}/${version}"
    versioned_bin="${version_dir}/metis"
    if [ -e "$version_dir" ] || [ -L "$version_dir" ]; then
        [ -d "$version_dir" ] && [ ! -L "$version_dir" ] \
            && [ -f "$versioned_bin" ] && [ ! -L "$versioned_bin" ] \
            || err "existing version path is not a managed version directory: ${version_dir}"
        [ "$(sha256_digest "$staged_bin")" = "$(sha256_digest "$versioned_bin")" ] \
            || err "existing ${version} binary differs from the verified release"
        rm -rf -- "$STAGING_DIR"
        STAGING_DIR=""
    else
        validate_managed_roots
        mv -- "$STAGING_DIR" "$version_dir"
        STAGING_DIR=""
    fi

    # Older installers placed a regular metis binary directly at the launcher
    # path. Preserve that verified Metis binary as the first rollback version
    # before atomically replacing the launcher with a managed symlink.
    migrate_legacy_launcher "$launcher" "$versions_dir" "$staging_root"

    validate_managed_roots
    [ -f "$versioned_bin" ] && [ ! -L "$versioned_bin" ] \
        || err "verified version binary is no longer a regular managed file: ${versioned_bin}"
    TEMP_LINK="${install_dir}/.metis-link-$$-${RANDOM}"
    ln -s "$versioned_bin" "$TEMP_LINK"
    mv -f -- "$TEMP_LINK" "$launcher"
    TEMP_LINK=""

    activated_output="$("$launcher" version 2>/dev/null)" \
        || err "activated launcher failed its version smoke test"
    activated_version="$(printf '%s\n' "$activated_output" | awk 'NR == 1 { print $1; exit }')"
    [ "${activated_version#v}" = "$version" ] \
        || err "activated launcher reports ${activated_version:-no-version}, expected ${resolved_tag}"

    prune_old_versions "$versions_dir" "$versioned_bin" "$locks_dir" "$launcher"

    log "installed: ${launcher} -> ${versioned_bin}"
    if ! printf '%s' ":$PATH:" | grep -q ":${install_dir}:"; then
        printf '\nNote: %s is not on your PATH. Add this to your shell rc:\n\n  export PATH="%s:$PATH"\n\n' \
            "$install_dir" "$install_dir"
    fi
    printf '%s\n' "$activated_output"

    if [ -z "$TOKEN" ]; then
        cat <<'EOF'

No GitHub token is required for public releases. If GitHub rate limits become
a problem, set METIS_GITHUB_TOKEN and run the installer again.

EOF
    fi
}

main "$@"
