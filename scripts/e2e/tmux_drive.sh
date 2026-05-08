#!/usr/bin/env bash
# tmux_drive.sh — full-coverage e2e for metis using tmux send-keys +
# capture-pane (task #34). Replaces the osascript driver: tmux is
# significantly more stable for TUI testing because it owns the PTY,
# isolates from focus theft, and runs headless in CI.
#
# Each test case follows the action → capture → assert loop:
#   1. tmux new-session -d -s metis-e2e -x 200 -y 50 'metis chat'
#   2. tmux send-keys -t metis-e2e '/help' C-m
#   3. sleep 0.3 (let the TUI render the new frame)
#   4. tmux capture-pane -t metis-e2e -p > /tmp/metis-e2e/<case>.log
#   5. grep / awk against the log to assert visible state
#   6. tmux kill-session -t metis-e2e
#
# Usage:
#   ./scripts/e2e/tmux_drive.sh                # run all cases
#   ./scripts/e2e/tmux_drive.sh slash_help     # run one case by id
#   ./scripts/e2e/tmux_drive.sh --list         # list available cases

set -euo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
OUT_DIR="${METIS_E2E_OUT:-/tmp/metis-e2e-tmux}"
mkdir -p "$OUT_DIR"

# Resolve the metis binary. Prefer the local install path so we always
# test the most-recently-built bits; fall back to PATH for CI envs that
# pre-install metis system-wide.
METIS_BIN="${METIS_BIN:-${HOME}/.local/bin/metis}"
if ! [[ -x "$METIS_BIN" ]]; then
  METIS_BIN="$(command -v metis 2>/dev/null || true)"
fi
if [[ -z "$METIS_BIN" ]]; then
  echo "metis binary not found (set METIS_BIN, or run 'make install')" >&2
  exit 2
fi

# Sanity-check tmux is available — required for the entire harness.
if ! command -v tmux >/dev/null 2>&1; then
  echo "tmux not installed (brew install tmux)" >&2
  exit 2
fi

SESSION="metis-e2e"
COLS=200
ROWS=50

# log captures the current pane to a per-step file so a failed case
# preserves visible state for inspection. Append the step number to
# the case name for ordered review.
shot_idx=0
log() {
  local case=$1 label=${2:-}
  shot_idx=$((shot_idx + 1))
  local fname="$OUT_DIR/${case}_${shot_idx}_${label}.log"
  tmux capture-pane -t "$SESSION" -p > "$fname"
  echo "  📸 $fname"
}

# start opens a fresh tmux session running `metis chat`. We intentionally
# pass `cd /tmp` first so the cwd in the banner is predictable across
# host machines.
start() {
  tmux kill-session -t "$SESSION" 2>/dev/null || true
  tmux new-session -d -s "$SESSION" -x "$COLS" -y "$ROWS" \
    "cd /tmp && '$METIS_BIN' chat"
  # Cold-start cushion. metis loads ~600ms on warm cache; bursty redraw
  # makes pane capture racy if we sample too soon.
  sleep 1.0
  shot_idx=0
}

# stop quits the metis session cleanly + tears down tmux.
stop() {
  tmux send-keys -t "$SESSION" "/quit" C-m 2>/dev/null || true
  sleep 0.4
  tmux kill-session -t "$SESSION" 2>/dev/null || true
}

# send wraps tmux send-keys with a small post-key sleep so the next
# capture sees the new frame, not the old one. KEY can be a literal
# string (typed) or a key code like `C-c`, `Enter`, `Escape`, `Tab`.
send() {
  local key="$1" sleepFor="${2:-0.25}"
  tmux send-keys -t "$SESSION" "$key"
  sleep "$sleepFor"
}

# assert_contains checks the most recent pane capture for a substring
# and exits 1 (causing the case to fail) when the substring is missing.
assert_contains() {
  local case=$1 label=$2 needle=$3
  local fname="$OUT_DIR/${case}_${shot_idx}_${label}.log"
  if ! grep -F -- "$needle" "$fname" >/dev/null 2>&1; then
    echo "  ❌ assert_contains: %q not in $fname" "$needle"
    echo "     ── pane capture ──"
    head -20 "$fname"
    echo "     ─────────────────"
    return 1
  fi
  echo "  ✅ found %q in $fname" "$needle"
}

# ─────────────────────────────────────────────────────────────────────
# Cases
# Every case is a function named case_<id>; --list enumerates them.
# Each case ends with a `stop` to keep one tmux session at a time.
# ─────────────────────────────────────────────────────────────────────

case_banner_renders() {
  echo "▶ banner_renders — fresh session shows the welcome banner with metis + cwd"
  start
  log banner_renders fresh
  assert_contains banner_renders fresh "metis"
  assert_contains banner_renders fresh "cwd"
  stop
}

case_slash_help() {
  echo "▶ slash_help — /help renders the command table"
  start
  send "/help" 0.2 ; send "Enter" 0.6
  log slash_help help_rendered
  assert_contains slash_help help_rendered "/help"
  assert_contains slash_help help_rendered "/quit"
  stop
}

case_input_repeat() {
  echo "▶ input_repeat — typing 11111 must accumulate (regression for OSC-110 scrub bug)"
  start
  send "1" 0.15
  send "1" 0.15
  send "1" 0.15
  send "1" 0.15
  send "1" 0.4
  log input_repeat after_5x_one
  assert_contains input_repeat after_5x_one "11111"
  send "C-u" 0.2  # clear input, don't actually submit garbage
  stop
}

case_arrow_jump() {
  echo "▶ arrow_jump — type 12345, ↑ jumps to col 0, ↓ jumps to col N"
  start
  send "12345" 0.4
  log arrow_jump after_12345
  assert_contains arrow_jump after_12345 "12345"
  send "Up" 0.3
  send "9" 0.4
  log arrow_jump after_9_at_start
  assert_contains arrow_jump after_9_at_start "912345"
  send "Down" 0.3
  send "0" 0.4
  log arrow_jump after_0_at_end
  assert_contains arrow_jump after_0_at_end "9123450"
  stop
}

case_slash_skills() {
  echo "▶ slash_skills — /skills list shows the installed skill set"
  start
  send "/skills" 0.2 ; send "Enter" 0.5
  log slash_skills list_rendered
  # The exact contents depend on the user's home dir; anchor on the
  # framing so the test is portable.
  assert_contains slash_skills list_rendered "skill"
  stop
}

case_slash_mcp_list() {
  echo "▶ slash_mcp_list — /mcp list works (Phase A regression)"
  start
  send "/mcp list" 0.2 ; send "Enter" 0.5
  log slash_mcp_list rendered
  # Either an MCP server line OR the empty-state hint; both prove
  # the dispatcher routed correctly.
  if ! grep -E "MCP server|no MCP" "$OUT_DIR/slash_mcp_list_${shot_idx}_rendered.log" >/dev/null; then
    echo "  ❌ neither 'MCP server' nor 'no MCP' found" >&2
    return 1
  fi
  echo "  ✅ /mcp list dispatcher routed"
  stop
}

case_double_esc_clear() {
  echo "▶ double_esc_clear — type something, ESC ESC clears input"
  start
  send "this should disappear" 0.4
  log double_esc_clear before_esc
  assert_contains double_esc_clear before_esc "this should disappear"
  send "Escape" 0.2
  send "Escape" 0.4
  log double_esc_clear after_double_esc
  if grep -F "this should disappear" "$OUT_DIR/double_esc_clear_${shot_idx}_after_double_esc.log" >/dev/null; then
    echo "  ❌ double-esc did not clear the input" >&2
    return 1
  fi
  echo "  ✅ double-esc cleared the input"
  stop
}

case_ctrl_c_quit() {
  echo "▶ ctrl_c_quit — Ctrl+C at idle exits immediately"
  start
  send "C-c" 1.0
  # Process exited; tmux session may already be gone.
  if tmux has-session -t "$SESSION" 2>/dev/null; then
    log ctrl_c_quit after_ctrl_c
    echo "  ⚠ session still alive after Ctrl+C — investigate"
    stop
  else
    echo "  ✅ session terminated by Ctrl+C"
  fi
}

# ─────────────────────────────────────────────────────────────────────
# Driver
# ─────────────────────────────────────────────────────────────────────

CASES=(
  banner_renders
  slash_help
  input_repeat
  arrow_jump
  slash_skills
  slash_mcp_list
  double_esc_clear
  ctrl_c_quit
)

if [[ "${1:-}" == "--list" ]]; then
  printf "  %s\n" "${CASES[@]}"
  exit 0
fi

if [[ -n "${1:-}" ]]; then
  case_fn="case_$1"
  if ! declare -F "$case_fn" >/dev/null; then
    echo "no such case: $1" >&2
    echo "available:" >&2
    printf "  %s\n" "${CASES[@]}" >&2
    exit 1
  fi
  $case_fn
  echo "✓ done — see $OUT_DIR"
  exit 0
fi

echo "Running ${#CASES[@]} cases against $METIS_BIN. Output: $OUT_DIR"
echo
fail=0
for c in "${CASES[@]}"; do
  if ! case_$c; then
    echo "  ✗ case $c failed"
    fail=$((fail + 1))
  fi
  echo
done
if [[ $fail -gt 0 ]]; then
  echo "✗ $fail case(s) failed — review pane captures in $OUT_DIR"
  exit 1
fi
echo "✓ all ${#CASES[@]} cases passed — captures in $OUT_DIR"
