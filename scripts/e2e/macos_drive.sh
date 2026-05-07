#!/usr/bin/env bash
# macos_drive.sh — Terminal.app + osascript driver for metis e2e checks.
# Each test case opens a fresh Terminal.app window, runs `metis chat`,
# sends a sequence of keystrokes via System Events, screen-captures the
# result, and tags the screenshot for later review.
#
# Why this over expect: expect drives a PTY directly, so the user
# never sees what's being tested. This script puts a real metis in a
# real Terminal.app window — exactly what the user uses day-to-day.
#
# Limitations (read these before adding cases):
#   * No mouse wheel — macOS System Events has no scroll API.
#   * No mouse drag with sub-cell precision — clickAt+dragAt rounds
#     to pixel coords, which is fine for buttons but flaky for word
#     selection in a TUI.
#   * Keystrokes go to whichever window is frontmost. If you click
#     outside the Terminal.app between cases, the next case's keys
#     vanish. The driver does `tell application "Terminal" to activate`
#     before every send, but a vigilant user can still steal focus.
#
# Usage:
#   ./scripts/e2e/macos_drive.sh                 # run all cases
#   ./scripts/e2e/macos_drive.sh slash_help      # run one case by id
#   ./scripts/e2e/macos_drive.sh --list          # list available cases

set -euo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
OUT_DIR="${METIS_E2E_OUT:-/tmp/metis-e2e}"
mkdir -p "$OUT_DIR"

# Cleanup any metis-chat process from a prior aborted run. The session
# lock files are unique-per-process so this just keeps the count of
# live metis chat REPLs sane.
cleanup_metis() {
  pkill -9 -f "metis chat" 2>/dev/null || true
  sleep 0.4
}

# launch_metis opens a fresh Terminal.app window and starts metis chat
# in /tmp. Waits for first paint via a fixed cushion (5s) — `expect`-
# style "look for auto mode" matching is unreliable when System Events
# is also delivering keystrokes in parallel.
# switch_to_english forces the macOS keyboard to a US/ABC input source.
# Without this, Chinese pinyin is left enabled and System Events
# keystrokes like "hello" get intercepted by the IME (the candidate
# bar pops up, ↑ becomes "previous candidate" instead of reaching
# metis). Best-effort: silently no-op if the source isn't installed.
switch_to_english() {
  osascript <<'OSA' >/dev/null 2>&1 || true
tell application "System Events"
    -- Cmd+Space toggles input source on most setups; safer is to
    -- call into the private Text Input API. We use a fallback that
    -- works for the default "ABC" / "U.S." source:
    try
        tell application id "com.apple.systemevents"
            keystroke "  " using {control down}
        end tell
    end try
end tell
OSA
  # The control+space combo above may or may not land on US-ABC
  # depending on user config. Most reliable path: ask the OS directly
  # via the `defaults` and `hidutil` toolchain — but that needs a
  # post-login session reload to take effect. For unattended e2e we
  # rely on the user having ABC as one of two enabled sources, so the
  # toggle lands on it.
}

launch_metis() {
  cleanup_metis
  switch_to_english
  osascript <<'OSA' >/dev/null
tell application "Terminal"
    activate
    do script "cd /tmp && metis chat"
end tell
OSA
  sleep 5
  switch_to_english
}

# send_keys feeds a list of "press X then sleep Y" pairs to whichever
# Terminal.app window is frontmost. Args alternate: KEY DELAY KEY DELAY...
# KEY can be a single char or a System Events keystroke spec like
# "return", "escape", or "ctrl down + c + ctrl up".
send_keys() {
  local script="tell application \"Terminal\" to activate\ndelay 0.3\ntell application \"System Events\"\n"
  while [[ $# -gt 0 ]]; do
    local key="$1" delay="${2:-0.2}"
    shift 2 || true
    case "$key" in
      return)   script+="    keystroke return\n" ;;
      escape)   script+="    key code 53\n" ;;
      up)       script+="    key code 126\n" ;;
      down)     script+="    key code 125\n" ;;
      tab)      script+="    keystroke tab\n" ;;
      "ctrl+c") script+="    keystroke \"c\" using control down\n" ;;
      "ctrl+t") script+="    keystroke \"t\" using control down\n" ;;
      "ctrl+s") script+="    keystroke \"s\" using control down\n" ;;
      "ctrl+r") script+="    keystroke \"r\" using control down\n" ;;
      "ctrl+l") script+="    keystroke \"l\" using control down\n" ;;
      *)
        # Plain printable string. Escape any double quotes.
        local escaped=${key//\"/\\\"}
        script+="    keystroke \"$escaped\"\n"
        ;;
    esac
    script+="    delay $delay\n"
  done
  script+="end tell"
  printf '%b' "$script" | osascript
}

# capture takes a screenshot of the entire screen (since we don't know
# Terminal.app window coords ahead of time) and writes it to OUT_DIR
# tagged with the case name + step number.
shot_idx=0
capture() {
  local case=$1 label=${2:-}
  shot_idx=$((shot_idx + 1))
  local fname="$OUT_DIR/${case}_${shot_idx}_${label}.png"
  screencapture -x "$fname"
  echo "  📷 $fname"
}

# Quit metis cleanly so the next case starts on a clean window.
quit_metis() {
  send_keys "/quit" 0.5 return 0.5
  sleep 1
}

# ─────────────────────────────────────────────────────────────────────────
# Cases
# Each case is a function named case_<id>. The id is what `--list` shows
# and what the CLI accepts as a single-case selector.
# ─────────────────────────────────────────────────────────────────────────

case_input_repeat() {
  echo "▶ input_repeat — type 11111 must accumulate (regression for OSC-110 scrub bug)"
  shot_idx=0
  launch_metis
  send_keys "1" 0.25 "1" 0.25 "1" 0.25 "1" 0.25 "1" 0.5
  capture "input_repeat" "after_5x_one"
  quit_metis
}

case_arrow_jump() {
  echo "▶ arrow_jump — type 12345, ↑ jumps to col 0, ↓ jumps to col N"
  # Pure digits dodge the macOS Chinese IME interception that latches
  # onto pinyin-shaped letter sequences like "hello". The arrow-jump
  # logic itself doesn't care about content — we just need a buffer
  # with a known start/end to verify cursor placement.
  shot_idx=0
  launch_metis
  send_keys "12345" 0.5
  capture "arrow_jump" "after_12345"
  send_keys up 0.5
  capture "arrow_jump" "after_up_at_col0"
  send_keys "9" 0.4
  capture "arrow_jump" "after_9_inserted_at_start"
  send_keys down 0.5
  capture "arrow_jump" "after_down_at_col_end"
  send_keys "0" 0.4
  capture "arrow_jump" "after_0_appended_at_end"
  quit_metis
}

case_slash_help() {
  echo "▶ slash_help — /help renders the help table"
  shot_idx=0
  launch_metis
  send_keys "/help" 0.5 return 0.8
  capture "slash_help" "help_rendered"
  quit_metis
}

case_slash_clear() {
  echo "▶ slash_clear — /clear wipes the transcript without quitting"
  shot_idx=0
  launch_metis
  send_keys "hello there this is some draft" 0.5
  capture "slash_clear" "before_clear"
  send_keys escape 0.4 escape 0.4
  capture "slash_clear" "after_double_esc"
  send_keys "/clear" 0.5 return 0.8
  capture "slash_clear" "after_clear"
  quit_metis
}

case_history_up() {
  echo "▶ history_up — empty input + ↑ recalls last prompt"
  shot_idx=0
  launch_metis
  send_keys "/help" 0.5 return 0.8
  send_keys up 0.6
  capture "history_up" "should_show_slash_help"
  send_keys escape 0.4
  quit_metis
}

case_taskpanel_toggle() {
  echo "▶ taskpanel_toggle — Ctrl+T opens task panel"
  shot_idx=0
  launch_metis
  send_keys "ctrl+t" 0.6
  capture "taskpanel_toggle" "panel_open"
  send_keys "ctrl+t" 0.6
  capture "taskpanel_toggle" "panel_closed"
  quit_metis
}

case_ctrl_c_quit() {
  echo "▶ ctrl_c_quit — Ctrl+C at idle should exit immediately"
  shot_idx=0
  launch_metis
  send_keys "ctrl+c" 0.5
  sleep 1
  capture "ctrl_c_quit" "after_ctrl_c"
}

case_double_esc_clear() {
  echo "▶ double_esc_clear — type something, ESC ESC clears input"
  shot_idx=0
  launch_metis
  send_keys "this should disappear" 0.5
  capture "double_esc_clear" "before_esc"
  send_keys escape 0.3 escape 0.5
  capture "double_esc_clear" "after_double_esc"
  quit_metis
}

# ─────────────────────────────────────────────────────────────────────────
# Driver
# ─────────────────────────────────────────────────────────────────────────

CASES=(
  input_repeat
  arrow_jump
  slash_help
  slash_clear
  history_up
  taskpanel_toggle
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

echo "Running ${#CASES[@]} cases. Output: $OUT_DIR"
echo
for c in "${CASES[@]}"; do
  case_$c
  echo
done
echo "✓ all cases done — review screenshots in $OUT_DIR"
