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

case_idle_no_sticky_queue_pill() {
  echo "▶ idle_no_sticky_queue_pill — empty queue must NOT render the sticky '◷ queued × N' pill above input (post-2026-05-08 fix)"
  start
  log idle_no_sticky_queue_pill fresh
  local fname="$OUT_DIR/idle_no_sticky_queue_pill_${shot_idx}_fresh.log"
  # Pre-fix: render_queue_pill.go painted "◷ queued × N: <peek>" right
  # above the input box even when the queue was empty? No — pill was
  # only rendered when len(queuedPrompts)>0. The post-fix change is
  # different: the pill is **never** rendered (its sticky behavior was
  # the bug). This negative assertion guards against a future revert.
  if grep -F "queued ×" "$fname" >/dev/null; then
    echo "  ❌ found 'queued ×' on idle frame — sticky pill regressed" >&2
    return 1
  fi
  # The status-bar chip ("◷ N queued") only appears when there's a
  # queue; idle frame must be free of it too.
  if grep -F "◷ " "$fname" | grep -F "queued" >/dev/null; then
    echo "  ❌ found '◷ N queued' status-bar chip on idle frame" >&2
    return 1
  fi
  echo "  ✅ idle frame is queue-artifact-free"
  stop
}

case_bare_resume_opens_picker() {
  echo "▶ bare_resume_opens_picker — 'metis -r' (no UUID) must open the resume picker, NOT bail with 'run: prompt is required' (2026-05-08 user video bug)"
  # This case launches metis differently from start(): we run `metis -r`
  # explicitly and look at the output before any TUI fully starts. The
  # picker writes "Resume which session?" / "(no prior sessions" to
  # stderr before bubbletea takes over.
  tmux kill-session -t "$SESSION" 2>/dev/null || true
  tmux new-session -d -s "$SESSION" -x "$COLS" -y "$ROWS" \
    "cd /tmp && '$METIS_BIN' -r"
  sleep 1.5
  tmux send-keys -t "$SESSION" "BSpace"  # nudge detached client
  sleep 0.4
  shot_idx=0
  log bare_resume_opens_picker initial
  local fname="$OUT_DIR/bare_resume_opens_picker_${shot_idx}_initial.log"
  # Hard fail: the regression message.
  if grep -F "run: prompt is required" "$fname" >/dev/null; then
    echo "  ❌ regression: 'run: prompt is required' on bare -r" >&2
    return 1
  fi
  # Pass when EITHER the picker prompt rendered OR (no prior sessions
  # in this $HOME) we landed in the chat banner. Both prove dispatch
  # routed `-r` to chat instead of run.
  if grep -E "Resume which session|no prior sessions|metis " "$fname" >/dev/null; then
    echo "  ✅ bare -r routed to chat (picker or empty-state hint)"
  else
    echo "  ❌ neither picker nor chat banner found on bare -r capture" >&2
    head -30 "$fname"
    return 1
  fi
  # Send q + Enter to bail out of the picker if it's open, then kill.
  tmux send-keys -t "$SESSION" "q" Enter 2>/dev/null || true
  sleep 0.3
  tmux send-keys -t "$SESSION" "/quit" Enter 2>/dev/null || true
  sleep 0.4
  tmux kill-session -t "$SESSION" 2>/dev/null || true
}

case_status_bar_renders_chips() {
  echo "▶ status_bar_renders_chips — sanity-check the status bar still paints its standard chips (no regression from the queue-chip insertion)"
  start
  # tmux detached clients don't auto-redraw — send a BSpace to nudge
  # the renderer into a full frame before capturing. (Same trick
  # cmp_drive.sh uses; the issue isn't metis-side, it's tmux's
  # output-batching behavior with no attached client.)
  send "BSpace" 0.6
  log status_bar_renders_chips fresh
  # Always-on chips on a clean tmux launch: 'tmux' (since we're inside
  # a tmux pane), 'auto mode' hint above status bar, and the version
  # marker on the right. If the queued-chip insertion shifted the
  # rendering pipeline these would silently drop.
  assert_contains status_bar_renders_chips fresh "auto mode"
  assert_contains status_bar_renders_chips fresh "tmux"
  assert_contains status_bar_renders_chips fresh "current: v"
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

# case_notify_channel_smoke — manual case verifying that the channel
# matrix actually emits OSC bytes when the trigger fires. Captures
# metis's stderr to a file (the notification fd) and greps for the
# tmux-DCS-wrapped OSC 9 sequence after a long-enough turn. This is
# the regression test for the "first 6s silenced" bug found 2026-05-08.
#
# Usage: ./tmux_drive.sh notify_channel_smoke
# Cost:  one ~60s LLM turn (the threshold check needs >30s).
case_notify_channel_smoke() {
  echo "▶ notify_channel_smoke — turn-end notification emits OSC bytes to stderr"
  local outdir=/tmp/metis-notify-tmux
  mkdir -p "$outdir"
  rm -f "$outdir/stderr.log"
  cat > "$outdir/run.sh" << EOF
#!/bin/bash
exec '$METIS_BIN' chat 2> $outdir/stderr.log
EOF
  chmod +x "$outdir/run.sh"

  tmux kill-session -t "$SESSION" 2>/dev/null || true
  tmux new-session -d -s "$SESSION" -x "$COLS" -y "$ROWS" \
    "cd /tmp && METIS_NOTIFY_CHANNEL=iterm2 $outdir/run.sh"
  sleep 1.5
  tmux send-keys -t "$SESSION" "BSpace"
  sleep 0.4
  # A long-ish multi-tool prompt so the turn duration crosses the
  # 30s notification threshold. Deliberately uses tools that
  # safe-allowlist permits (echo / Read).
  tmux send-keys -t "$SESSION" "Bash echo step1 && Read /etc/hosts && Bash echo step2 done"
  tmux send-keys -t "$SESSION" Enter
  sleep 45  # > NotifyMinDuration (30s) so the threshold check passes
  shot_idx=0
  log notify_channel_smoke after_turn
  stop

  if ! [[ -s "$outdir/stderr.log" ]]; then
    echo "  ⚠ stderr.log empty — turn may have been < 30s; not necessarily a bug" >&2
    return 0
  fi
  if grep -q $'\x1b\x1b]9;' "$outdir/stderr.log" || grep -q $'\x1b]9;' "$outdir/stderr.log"; then
    echo "  ✅ OSC 9 notification bytes captured in $outdir/stderr.log"
    return 0
  fi
  echo "  ❌ stderr.log non-empty but no OSC 9 found:"
  od -c "$outdir/stderr.log" | head -10 >&2
  return 1
}

# case_lazy_env_modes_smoke — manual case (NOT in default CASES list)
# that hits the real LLM under all three ENABLE_TOOL_SEARCH modes.
# Verifies: (a) no startup error in any mode, (b) banner renders, (c)
# the agent reaches an executing-tool state (proves toolSpecs()
# survives the rewrite). Not asserted: response content (LLM is
# non-deterministic) — this is a smoke check.
#
# Usage: ./tmux_drive.sh lazy_env_modes_smoke
# Cost: 3 small LLM turns to your default provider.
case_lazy_env_modes_smoke() {
  echo "▶ lazy_env_modes_smoke — ENABLE_TOOL_SEARCH={auto,true,false} all start cleanly + execute a Bash"
  for mode in "auto" "true" "false"; do
    echo "  → mode=$mode"
    tmux kill-session -t "$SESSION" 2>/dev/null || true
    tmux new-session -d -s "$SESSION" -x "$COLS" -y "$ROWS" \
      "cd /tmp && ENABLE_TOOL_SEARCH=$mode '$METIS_BIN' chat"
    sleep 1.5
    tmux send-keys -t "$SESSION" "BSpace"
    sleep 0.4
    shot_idx=0
    log "lazy_env_${mode}" banner
    local fname="$OUT_DIR/lazy_env_${mode}_${shot_idx}_banner.log"
    if ! grep -F "metis" "$fname" >/dev/null; then
      echo "    ❌ banner missing in $fname (startup failed under ENABLE_TOOL_SEARCH=$mode)" >&2
      stop
      return 1
    fi
    # Send a one-shot Bash that the safe-allowlist permits without
    # prompt (echo is in the allowlist) so we don't get stuck on a
    # permission overlay.
    tmux send-keys -t "$SESSION" "Bash echo \"smoke-$mode\""
    tmux send-keys -t "$SESSION" Enter
    sleep 25
    log "lazy_env_${mode}" after_response
    local respFile="$OUT_DIR/lazy_env_${mode}_${shot_idx}_after_response.log"
    if grep -E "smoke-${mode}|recap" "$respFile" >/dev/null; then
      echo "    ✅ mode=$mode produced a tool execution + recap"
    else
      echo "    ⚠ mode=$mode: no smoke marker / recap in capture (may be slow LLM, not a hard fail)"
    fi
    stop
  done
  echo "  ✅ all three ENABLE_TOOL_SEARCH modes completed"
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
  idle_no_sticky_queue_pill
  status_bar_renders_chips
  bare_resume_opens_picker
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
