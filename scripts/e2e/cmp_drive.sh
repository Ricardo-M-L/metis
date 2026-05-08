#!/usr/bin/env bash
# cmp_drive.sh — side-by-side claude code vs metis comparison for the
# 60-item parity push (task #39). Each test point is a function that
# (a) starts matching tmux sessions for claude + metis, (b) sends
# comparable input, (c) captures both panes, (d) appends a section to
# /tmp/metis-cmp-issues.md describing what each side did and where
# they diverge.
#
# This script DOES NOT fix anything. The user explicitly asked for
# "先记录全部差异再统一修再统一回归" — issues are written, fixes
# happen in a follow-up pass.
#
# Usage:
#   scripts/e2e/cmp_drive.sh                # run every test point
#   scripts/e2e/cmp_drive.sh slash_help     # run a single test point
#   scripts/e2e/cmp_drive.sh --list         # list available points

set -uo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
ISSUES="${METIS_CMP_ISSUES:-/tmp/metis-cmp-issues.md}"
CAPTURES="${METIS_CMP_CAPTURES:-/tmp/metis-cmp-captures}"
mkdir -p "$CAPTURES"

CLAUDE_BIN="${CLAUDE_BIN:-$(command -v claude || true)}"
METIS_BIN="${METIS_BIN:-${HOME}/.local/bin/metis}"
[[ -x "$METIS_BIN" ]] || METIS_BIN="$(command -v metis || true)"

if [[ -z "${CLAUDE_BIN}" ]]; then
  echo "claude not found — install Claude Code or set CLAUDE_BIN" >&2
  exit 2
fi
if [[ -z "${METIS_BIN}" ]]; then
  echo "metis not found — run 'make install'" >&2
  exit 2
fi

# Reset issues file at start of full run.
init_issues() {
  cat > "$ISSUES" <<EOF
# metis vs claude code — comparison findings

Generated: $(date -Iseconds)
metis: $($METIS_BIN version 2>&1 | head -1)
claude: $($CLAUDE_BIN --version 2>&1 | head -1)
captures: $CAPTURES

Each section records:
- **point**: what was tested
- **claude**: relevant excerpt from claude's pane
- **metis**: relevant excerpt from metis's pane
- **diff**: behavioral or visual difference
- **severity**: blocker / cosmetic / metis-only-feature

EOF
}

# wait_ready polls a tmux pane until it shows expected text. tmux
# detached sessions don't auto-refresh; sending BSpace nudges the pane.
wait_ready() {
  local sess=$1 needle=$2 timeout=${3:-15}
  local deadline=$(($(date +%s) + timeout))
  while [[ $(date +%s) -lt $deadline ]]; do
    tmux send-keys -t "$sess" BSpace 2>/dev/null
    if tmux capture-pane -t "$sess" -p 2>/dev/null | grep -qF "$needle"; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}

# capture writes a pane snapshot to disk and stdout (path).
capture() {
  local sess=$1 label=$2
  local fname="$CAPTURES/${label}.txt"
  # tmux detached sessions hold a stale buffer until a key arrives.
  # Send BSpace twice with a sleep gap — once to flush any pending
  # frame, again to settle. Without this many captures came back
  # blank during the first comparison run.
  tmux send-keys -t "$sess" BSpace 2>/dev/null
  sleep 0.4
  tmux send-keys -t "$sess" BSpace 2>/dev/null
  sleep 0.4
  tmux capture-pane -t "$sess" -p -e -S -200 > "$fname" 2>/dev/null
  echo "$fname"
}

# record_section appends a Markdown section to the issues file.
# Args: point_name claude_excerpt metis_excerpt diff_note severity
record_section() {
  local name=$1 cl=$2 mt=$3 diff=$4 sev=$5
  cat >> "$ISSUES" <<EOF

## ${name}

**severity:** ${sev}

**diff:** ${diff}

\`\`\`
[claude]
$(echo "$cl" | head -25)
\`\`\`

\`\`\`
[metis]
$(echo "$mt" | head -25)
\`\`\`

EOF
}

# Pure-metis check (no claude side-by-side); used for features metis
# claims to add that don't exist in claude.
record_metis_only() {
  local name=$1 mt=$2 verdict=$3
  cat >> "$ISSUES" <<EOF

## ${name} (metis-only)

**verdict:** ${verdict}

\`\`\`
[metis]
$(echo "$mt" | head -25)
\`\`\`

EOF
}

start_claude() {
  local sess=$1 ; shift
  tmux kill-session -t "$sess" 2>/dev/null
  # Claude Code: bypass perms equivalent so mid-test prompts don't hang.
  tmux new-session -d -s "$sess" -x 200 -y 50 "cd /tmp && '$CLAUDE_BIN' --dangerously-skip-permissions"
}

start_metis() {
  local sess=$1 ; shift
  tmux kill-session -t "$sess" 2>/dev/null
  tmux new-session -d -s "$sess" -x 200 -y 50 "cd /tmp && '$METIS_BIN' chat --dangerously-skip-permissions"
}

stop_session() {
  local sess=$1
  tmux kill-session -t "$sess" 2>/dev/null
}

# ============================================================================
# Test points
# Each follows: start sessions → wait ready → send input → capture → record
# Naming: <area>__<feature> so similar tests group in --list output.
# ============================================================================

# UI / banner — visual comparison only
case_ui__welcome_banner() {
  echo "▶ ui__welcome_banner"
  start_claude cmp-cc-banner
  start_metis cmp-mt-banner
  wait_ready cmp-cc-banner "Claude" 20 || true
  wait_ready cmp-mt-banner "metis" 20 || true
  local cc=$(capture cmp-cc-banner ui__welcome_banner__claude)
  local mt=$(capture cmp-mt-banner ui__welcome_banner__metis)
  record_section "ui__welcome_banner" \
    "$(cat "$cc" | head -12)" \
    "$(cat "$mt" | head -12)" \
    "Manual: compare icon weight, border style, model+cwd row presence." \
    "cosmetic"
  stop_session cmp-cc-banner
  stop_session cmp-mt-banner
}

case_ui__version_string() {
  echo "▶ ui__version_string"
  local cc=$($CLAUDE_BIN --version 2>&1 | head -1)
  local mt=$($METIS_BIN version 2>&1 | head -1)
  record_section "ui__version_string" \
    "$cc" "$mt" \
    "Both should print short semver. metis prints git-describe noise (-21-gab7a825-dirty). Status bar separately uses shortSemver()." \
    "cosmetic"
}

case_slash__help() {
  echo "▶ slash__help"
  start_claude cmp-cc-help
  start_metis cmp-mt-help
  wait_ready cmp-cc-help "Claude" 20 || true
  wait_ready cmp-mt-help "metis" 20 || true
  tmux send-keys -t cmp-cc-help "/help" Enter ; sleep 1
  tmux send-keys -t cmp-mt-help "/help" Enter ; sleep 1
  local cc=$(capture cmp-cc-help slash__help__claude)
  local mt=$(capture cmp-mt-help slash__help__metis)
  record_section "slash__help" \
    "$(grep -E '^\s*/' "$cc" | head -20)" \
    "$(grep -E '^\s*/' "$mt" | head -20)" \
    "Both render a slash-command table. Compare command coverage." \
    "cosmetic"
  stop_session cmp-cc-help ; stop_session cmp-mt-help
}

case_slash__model() {
  echo "▶ slash__model"
  start_metis cmp-mt-model
  wait_ready cmp-mt-model "metis" 20 || true
  tmux send-keys -t cmp-mt-model "/model" Enter ; sleep 1
  local mt=$(capture cmp-mt-model slash__model__metis)
  record_metis_only "slash__model" \
    "$(tail -10 "$mt")" \
    "Should display model name. claude has /model with provider switcher; metis has model display + switch."
  stop_session cmp-mt-model
}

# Phase A — /mcp subcommands (metis-only superset)
case_slash__mcp_subcommands() {
  echo "▶ slash__mcp_subcommands"
  start_metis cmp-mt-mcp
  wait_ready cmp-mt-mcp "metis" 20 || true
  for sub in list reload "unknown_subcmd"; do
    tmux send-keys -t cmp-mt-mcp "/mcp $sub" Enter ; sleep 1
  done
  local mt=$(capture cmp-mt-mcp slash__mcp_subcommands__metis)
  record_metis_only "slash__mcp_subcommands" \
    "$(grep -A1 -E 'mcp ' "$mt" | head -15)" \
    "list/reload should succeed; unknown_subcmd should show usage hint with full subcommand list."
  stop_session cmp-mt-mcp
}

# Phase A — /skills subcommands (metis-only superset)
case_slash__skills_subcommands() {
  echo "▶ slash__skills_subcommands"
  start_metis cmp-mt-skills
  wait_ready cmp-mt-skills "metis" 20 || true
  tmux send-keys -t cmp-mt-skills "/skills frobnicate" Enter ; sleep 1
  tmux send-keys -t cmp-mt-skills "/skills" Enter ; sleep 1
  local mt=$(capture cmp-mt-skills slash__skills_subcommands__metis)
  record_metis_only "slash__skills_subcommands" \
    "$(tail -25 "$mt")" \
    "Unknown subcmd shows usage. Bare /skills lists installed skills."
  stop_session cmp-mt-skills
}

# Phase B — short flags (-c / -r / -d / --bare / --dangerously-skip-permissions)
case_cli__short_flags() {
  echo "▶ cli__short_flags"
  local r_test bare_test
  r_test=$($METIS_BIN -r ghost-id --help 2>&1 | head -1)
  bare_test=$($METIS_BIN --bare --help 2>&1 | head -1)
  local cont_test=$($METIS_BIN -c --help 2>&1 | head -1)
  local danger_test=$($METIS_BIN --dangerously-skip-permissions --help 2>&1 | head -1)
  record_metis_only "cli__short_flags" \
"-r ghost-id  → $r_test
--bare       → $bare_test
-c           → $cont_test
--dangerously-skip-permissions → $danger_test" \
    "All four should show 'Usage of metis' (flag accepted) — original bug was '-r flag provided but not defined'."
}

# Phase C — high-frequency slashes
case_slash__phase_c_set() {
  echo "▶ slash__phase_c_set"
  start_metis cmp-mt-phc
  wait_ready cmp-mt-phc "metis" 20 || true
  for s in "/copy" "/insights" "/output-style" "/break-cache" "/security-review" "/feedback"; do
    tmux send-keys -t cmp-mt-phc "$s" Enter ; sleep 0.8
  done
  local mt=$(capture cmp-mt-phc slash__phase_c_set__metis)
  record_metis_only "slash__phase_c_set" \
    "$(tail -40 "$mt")" \
    "Each slash should produce non-error output. Look for 'unknown' which would mean registration broke."
  stop_session cmp-mt-phc
}

# Phase D — Ctrl+G external editor
case_keybind__ctrl_g_editor() {
  echo "▶ keybind__ctrl_g_editor"
  # We don't actually open the editor — just verify Ctrl+G doesn't crash
  # the session. Setting EDITOR=true makes the spawn return immediately.
  start_metis cmp-mt-ctrlg
  wait_ready cmp-mt-ctrlg "metis" 20 || true
  tmux send-keys -t cmp-mt-ctrlg "this is a draft prompt"
  sleep 0.5
  tmux send-keys -t cmp-mt-ctrlg "C-g" ; sleep 3
  # Ctrl+G launched $EDITOR (vim by default) on a temp file. Quit
  # without saving so we capture metis's post-editor state, not
  # vim's modeline.
  tmux send-keys -t cmp-mt-ctrlg "Escape" ; sleep 0.3
  tmux send-keys -t cmp-mt-ctrlg ":q!" Enter ; sleep 1.5
  local mt=$(capture cmp-mt-ctrlg keybind__ctrl_g_editor__metis)
  record_metis_only "keybind__ctrl_g_editor" \
    "$(tail -10 "$mt")" \
    "Session should survive Ctrl+G. Tests assert tea.ExecProcess hand-off doesn't crash."
  stop_session cmp-mt-ctrlg
}

# Phase E — metis ps / logs
case_cli__ps_logs() {
  echo "▶ cli__ps_logs"
  local ps_out
  ps_out=$($METIS_BIN ps --limit 3 2>&1)
  record_metis_only "cli__ps_logs" \
    "$ps_out" \
    "Should list ID/CREATED/MODEL/BYTES/TITLE/PID columns; PID '-' for foreground sessions."
}

# Phase F — Ctrl+X shell mode toggle
case_keybind__ctrl_x_shell() {
  echo "▶ keybind__ctrl_x_shell"
  start_metis cmp-mt-ctrlx
  wait_ready cmp-mt-ctrlx "metis" 20 || true
  tmux send-keys -t cmp-mt-ctrlx "C-x" ; sleep 1
  local mt=$(capture cmp-mt-ctrlx keybind__ctrl_x_shell__metis)
  record_metis_only "keybind__ctrl_x_shell" \
    "$(grep -i shell "$mt" | head -3)" \
    "Should show 'shell mode: on' info row. Submit-side dispatch is still pending — this just verifies the toggle keybind."
  stop_session cmp-mt-ctrlx
}

# Phase F — /thinkback /ultraplan /onboarding
case_slash__phase_f_set() {
  echo "▶ slash__phase_f_set"
  start_metis cmp-mt-phf
  wait_ready cmp-mt-phf "metis" 20 || true
  for s in "/thinkback" "/ultraplan refactor the auth module" "/onboarding"; do
    tmux send-keys -t cmp-mt-phf "$s" Enter ; sleep 0.8
  done
  local mt=$(capture cmp-mt-phf slash__phase_f_set__metis)
  record_metis_only "slash__phase_f_set" \
    "$(tail -30 "$mt")" \
    "Each should produce non-error output. /thinkback says 'no thinking trace' on empty session."
  stop_session cmp-mt-phf
}

# UI — queued message pill
case_ui__queued_pill() {
  echo "▶ ui__queued_pill"
  # Stand up metis, fake a turn-active state by sending a long-running
  # prompt then immediately a second one. The mid-turn second prompt
  # should land in m.queuedPrompts and appear as a pill above the input.
  start_metis cmp-mt-queue
  wait_ready cmp-mt-queue "metis" 20 || true
  # Send a prompt that spawns work...
  tmux send-keys -t cmp-mt-queue "say something long" Enter ; sleep 1
  # ...then queue a second one mid-turn
  tmux send-keys -t cmp-mt-queue "this should queue" Enter ; sleep 2
  local mt=$(capture cmp-mt-queue ui__queued_pill__metis)
  record_metis_only "ui__queued_pill" \
    "$(grep -i 'queue' "$mt" | head -5)" \
    "Look for '◷ queued × N' pill above input box, OR '(queued · …)' info row in transcript."
  stop_session cmp-mt-queue
}

# ============================================================================
# Driver
# ============================================================================

CASES=(
  ui__welcome_banner
  ui__version_string
  slash__help
  slash__model
  slash__mcp_subcommands
  slash__skills_subcommands
  cli__short_flags
  slash__phase_c_set
  keybind__ctrl_g_editor
  cli__ps_logs
  keybind__ctrl_x_shell
  slash__phase_f_set
  ui__queued_pill
)

if [[ "${1:-}" == "--list" ]]; then
  printf "  %s\n" "${CASES[@]}"
  exit 0
fi

if [[ -n "${1:-}" ]]; then
  init_issues
  case_fn="case_$1"
  if ! declare -F "$case_fn" >/dev/null; then
    echo "no such case: $1" >&2
    echo "available:" >&2
    printf "  %s\n" "${CASES[@]}" >&2
    exit 1
  fi
  $case_fn
  echo "✓ $1 → captures in $CAPTURES, issues in $ISSUES"
  exit 0
fi

init_issues
echo "Running ${#CASES[@]} comparison cases."
echo "claude: $CLAUDE_BIN"
echo "metis:  $METIS_BIN"
echo "captures: $CAPTURES"
echo "issues:   $ISSUES"
echo
for c in "${CASES[@]}"; do
  case_$c || echo "  ⚠ $c crashed (continuing)"
done
echo
echo "✓ done — $ISSUES holds the diff catalog"
