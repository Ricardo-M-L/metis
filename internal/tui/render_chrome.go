package tui

// render_chrome.go — bottom-of-screen UI: input box, spinner, status
// bar, and hints. These are the "always-visible" rows below the
// scrollable transcript.

import (
	"fmt"
	"image/color"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/version"
)

// renderInputLine paints the textarea + dividers. Flat style (no
// rounded border): a horizontal rule above and below, "  > " prompt
// inside. claude-code parity — the boxed look is too heavy on dark
// terminals.
func renderInputLine(m *Model) string {
	// Height is managed by the textarea itself (DynamicHeight=true,
	// configured in NewModel). It tracks **visual** lines after wrap,
	// so a long single-line message that wraps into 3 rows reports
	// Height()=3 and renders as 3 rows here. The outer chrome counter
	// in tui_render.go reads strings.Count(lower, "\n"), which already
	// accounts for those wrapped rows because they're emitted as real
	// "\n"-separated lines in our loop below.

	termW := m.width
	if termW <= 0 {
		termW = 80
	}
	// Input width = terminal - 4 (2 left padding, 2 right padding/cursor
	// breathing room). No max-cap: the previous 100-char ceiling caused
	// premature wrap on wide terminals when the user pasted CJK + paths
	// (each CJK glyph is 2 cells, so a 50-char Chinese line already
	// blows the cap). Wide terminals SHOULD fill, otherwise the user's
	// pasted content wraps in awkward places (image #10 bug 2026-05-09).
	// 20-cell floor keeps a sub-30-col terminal usable.
	textareaW := termW - 4
	if textareaW < 20 {
		textareaW = 20
	}
	if m.input.Width() != textareaW {
		m.input.SetWidth(textareaW)
	}
	body := m.input.View()
	body = strings.TrimRight(body, "\n")

	dividerW := termW - 2
	if dividerW < 20 {
		dividerW = 20
	}
	divider := styleMuted.Render("  " + strings.Repeat("─", dividerW))

	var s strings.Builder
	s.WriteString("\n")
	s.WriteString(divider)
	s.WriteString("\n")
	for _, ln := range strings.Split(body, "\n") {
		s.WriteString("  ")
		s.WriteString(ln)
		s.WriteString("\n")
	}
	s.WriteString(divider)
	s.WriteString("\n")
	return s.String()
}

// renderSpinnerStatus builds the streaming-state line:
//
//   - Verb sub (12s · ↓ 3.1k tokens · thought for 1s)
//
// All bracketed parts are conditional. The leading "Verb" is shimmered
// from 3s onward so quick replies don't flicker the dimming animation.
func renderSpinnerStatus(m *Model) string {
	elapsed := time.Since(m.spinnerStartedAt)
	// Glyph advance is **time-gated**, not tick-gated. claude-code's
	// spinner advances one frame every 120ms via
	// `Math.floor(time / 120)`; we do the same so a faster TUI tick
	// (40ms / 25fps default) doesn't make the asterisk flicker. The
	// user reported "闪得太快" — that was every-tick rotation.
	const spinnerStepMs = 120
	frameIdx := int(elapsed.Milliseconds()/spinnerStepMs) % len(spinnerFrames)
	frame := spinnerFrames[frameIdx]

	// elapsedDisplay decides which clock the user sees. Default is the
	// turn clock (since spinnerStartedAt). When a tool is in flight
	// (spinnerSub non-empty), switch to the **tool's own** start time
	// so a long-running bash like `git rebase` reads as
	// `Bash · executing · git rebase (12s …)` instead of
	// `Bash · executing · git rebase (1h 18m …)` — that latter form
	// (the 2026-05-08 video bug) misleads the user into thinking a
	// trivial `cd` ran for an hour, when in fact the turn had been
	// looping for an hour and the tool itself just started.
	elapsedDisplay := elapsed
	if m.spinnerSub != "" {
		for i := len(m.toolEvents) - 1; i >= 0; i-- {
			if m.toolEvents[i].Kind == "start" {
				elapsedDisplay = time.Since(m.toolEvents[i].StartTime)
				break
			}
		}
	}

	var parts []string
	parts = append(parts, formatElapsed(elapsedDisplay))
	// Spinner row shows ONE direction at a time, switching by phase
	// (claude-code style — user feedback images #17-19):
	//   ↑ N tokens — uploading / waiting for first stream chunk
	//   ↓ N tokens — currently receiving stream deltas (text or thinking)
	// The arrow flips back to ↑ at the start of each iteration (after
	// a tool call when the next API call's prompt is being sent).
	//
	// Source of truth: explicit spinnerPhase, set in handleAgentEvent
	// (mirrors claude-code's SpinnerMode enum from
	// SpinnerAnimationRow.tsx). When unset (legacy callers / tests
	// that don't drive events) fall back to the firstStreamAt + buffer
	// heuristic so historical behavior holds.
	var receiving bool
	switch m.spinnerPhase {
	case "thinking", "responding", "tool", "tool-input", "tool-use":
		receiving = true
	case "requesting":
		receiving = false
	default:
		receiving = !m.firstStreamAt.IsZero() && (m.streamingText != "" || m.thinkingText != "")
	}
	if receiving {
		// Live estimate: chars/4 ≈ tokens (rough). Beats waiting for
		// EventTokens to fire at end of stream, which leaves the
		// counter visibly stuck mid-stream.
		out := m.totalTokens.LastOut()
		if est := (len(m.streamingText) + len(m.thinkingText)) / 4; est > out {
			out = est
		}
		if out > 0 {
			parts = append(parts, fmt.Sprintf("↓ %s tokens", formatTokens(out)))
		}
	} else if in := m.totalTokens.LastIn(); in > 0 {
		parts = append(parts, fmt.Sprintf("↑ %s tokens", formatTokens(in)))
	}
	if !m.firstStreamAt.IsZero() {
		thought := m.firstStreamAt.Sub(m.spinnerStartedAt)
		if thought >= time.Second {
			parts = append(parts, fmt.Sprintf("thought for %ds", int(thought.Seconds())))
		}
	}

	var s strings.Builder
	s.WriteString("\n")
	// Phase F Ctrl+B (2026-05-12) — backgrounded turn shrinks to a
	// one-line chip showing only the spinner + "(background Xs)".
	// We still want the spinner glyph + elapsed clock so the user
	// can confirm the turn is alive; everything else (verb / args
	// preview / token chip) hides until the turn is foregrounded
	// again or finalizeTurn fires.
	if m.turnBackgrounded {
		bgElapsed := time.Since(m.backgroundedAt).Truncate(time.Second)
		s.WriteString(styleAccent.Render("  " + frame + " "))
		s.WriteString(styleDim.Render(fmt.Sprintf("background %s · Ctrl+B to foreground · Ctrl+C cancels", bgElapsed)))
		s.WriteString("\n")
		return s.String()
	}
	s.WriteString(styleAccent.Render("  " + frame + " "))
	switch {
	case m.spinnerOverride != "":
		// Compaction (or any other long-running blocking phase) — the
		// override label wins over the verb so the user sees what's
		// actually happening, not a generic thinking verb. Token
		// counter intentionally omitted: the OSC 9;4 indeterminate
		// indicator on the terminal tab (set in tui_events.go on
		// EventCompactionStart) is the primary visual cue; an inline
		// token counter on top of that is redundant (user feedback
		// 2026-05-15).
		s.WriteString(shimmerStyle(elapsed).Render(m.spinnerOverride))
	case m.spinnerSub != "":
		s.WriteString(toolUseFlashStyle(elapsed).Render(m.spinnerVerb))
		s.WriteString(styleDim.Render(" · " + truncate(m.spinnerSub, 35)))
	default:
		s.WriteString(shimmerStyle(elapsed).Render(m.spinnerVerb))
	}
	if len(parts) > 0 {
		// Status info (elapsed · tokens · thought) — readable in dim,
		// not buried in muted grey. Parens themselves stay muted as
		// punctuation so the eye reads through them.
		s.WriteString(styleMuted.Render(" ("))
		s.WriteString(styleDim.Render(strings.Join(parts, " · ")))
		s.WriteString(styleMuted.Render(")"))
	}
	s.WriteString("\n")
	// Compaction extras — match claude-code's image #19 layout: spinner
	// row + progress bar with % + sub-line announcing the auto-window
	// threshold and the configure command. Only emitted when the
	// override label indicates we're in a compaction phase.
	if strings.HasPrefix(m.spinnerOverride, "Compacting conversation") {
		s.WriteString(renderCompactionExtras(m))
	}
	// Dreaming extras (Phase C 2026-05-16) — parallel chrome under
	// the spinner during auto-memory consolidation. We don't have a
	// reliable byte target the way compaction does (the summarize
	// stream's length is unknown), so we skip the progress bar and
	// just emit a single sub-line announcing what's happening + the
	// configure entry point.
	if strings.HasPrefix(m.spinnerOverride, "Dreaming") {
		s.WriteString(renderDreamingExtras(m))
	}
	return s.String()
}

// renderDreamingExtras draws the single sub-row that sits under the
// spinner during auto-memory consolidation:
//
//	└ Consolidating recent sessions · /dream status to inspect
//
// Mirrors the compaction-extras layout (image #19) for visual
// continuity, but omits the progress bar — dreaming has no stream
// length we can map to a percentage, and a sweeping/indeterminate bar
// would lie about progress more than it would help.
func renderDreamingExtras(m *Model) string {
	_ = m // reserved for future per-fork progress info (sessions touched)
	var s strings.Builder
	s.WriteString("  ")
	s.WriteString(styleDim.Render("└ Consolidating recent sessions · /dream status to inspect"))
	s.WriteString("\n")
	return s.String()
}

// renderCompactionExtras draws the two rows that sit under the spinner
// during LLM-driven compaction:
//
//	▰▰▰▰▰▰▰▰▰▱▱▱▱▱▱▱▱▱▱▱▱▱ 42%
//	└ Compacting at auto window (170k tokens) · /autocompact to configure
//
// The progress is a saturating estimate based on summarize-stream bytes
// (we don't know the final summary length until done); it caps at 95%
// so the user never sees "100%" while the call is still mid-flight. On
// EventContextCompacted the override clears and these rows disappear
// with it. Layout mirrors claude-code's compaction display (image #19
// user feedback 2026-05-15).
func renderCompactionExtras(m *Model) string {
	// 8000-byte heuristic target: summaries usually land between 4-12KB,
	// so bytes/80 puts a typical run at 50–95% by the time it finishes.
	pct := m.spinnerCompactionBytes / 80
	if pct > 95 {
		pct = 95
	}
	if pct < 0 {
		pct = 0
	}
	const barWidth = 22
	filled := pct * barWidth / 100
	if filled > barWidth {
		filled = barWidth
	}
	var bar strings.Builder
	for i := 0; i < filled; i++ {
		bar.WriteString("▰")
	}
	for i := filled; i < barWidth; i++ {
		bar.WriteString("▱")
	}

	// Auto-window threshold = Compactor.Threshold (default 0.85) ×
	// MaxContextTokens. Falls back to 85% of provider cap if the
	// Compactor isn't reachable (e.g. legacy callers / tests).
	autoWindow := 0
	if m.loop != nil {
		if m.loop.Compactor != nil && m.loop.Compactor.MaxContextTokens > 0 {
			thr := m.loop.Compactor.Threshold
			if thr <= 0 {
				thr = 0.85
			}
			autoWindow = int(float64(m.loop.Compactor.MaxContextTokens) * thr)
		} else if m.loop.Provider != nil {
			if cap := m.loop.Provider.MaxContextTokens(); cap > 0 {
				autoWindow = int(float64(cap) * 0.85)
			}
		}
	}

	var s strings.Builder
	s.WriteString("  ")
	s.WriteString(styleAccent.Render(bar.String()))
	s.WriteString(styleDim.Render(fmt.Sprintf(" %d%%", pct)))
	s.WriteString("\n")
	s.WriteString("  ")
	if autoWindow > 0 {
		s.WriteString(styleDim.Render(fmt.Sprintf("└ Compacting at auto window (%s tokens) · /autocompact to configure", formatTokens(autoWindow))))
	} else {
		s.WriteString(styleDim.Render("└ Compacting at auto window · /autocompact to configure"))
	}
	s.WriteString("\n")
	return s.String()
}

// renderStatusBar paints the bottom-of-chat divider line. Wall-clock
// elapsed + reasoning effort + fast-mode marker sit left, total token
// usage right-aligns to the terminal edge so the eye can find
// context-window pressure without scanning.
func renderStatusBar(m *Model) string {
	var s strings.Builder
	// Wall-clock elapsed indicator removed — the user flagged it as
	// noise (a dangling "├─ 0s" / "⏱ 0s" with no functional purpose).
	// claude-code doesn't ship one either; the spinner row already
	// surfaces per-turn duration when it matters.
	var leftParts []string
	if m.loop != nil {
		if g := effortGlyph(m.loop.Effort); g != "" {
			leftParts = append(leftParts, g+" "+string(m.loop.Effort))
		}
		if m.loop.Fast {
			leftParts = append(leftParts, "↯ fast")
		}
	}
	if label := m.vimModeLabel(); label != "" {
		leftParts = append(leftParts, label)
	}
	if os.Getenv("TMUX") != "" {
		leftParts = append(leftParts, "⊟ tmux")
	}
	if pr := prBadgeText(); pr != "" {
		leftParts = append(leftParts, pr)
	}
	if n := tasksRunningCount(m.sessionID); n > 0 {
		leftParts = append(leftParts, fmt.Sprintf("☰ %d todos", n))
	}
	// Background bash job pool — auto-promoted long-runners and
	// explicit run_in_background commands. Chip lights up when
	// any job is still alive so the user knows there's work
	// happening "off-screen" they can BashList / BashOutput.
	if n := bashJobsRunningCount(m); n > 0 {
		leftParts = append(leftParts, fmt.Sprintf("⚙ %d jobs", n))
	}
	if n := len(m.queuedPrompts); n > 0 {
		// Compact queue chip in the status bar (the sticky pill above
		// the input was removed). Always visible regardless of scroll
		// position, complements the in-stream "(queued × N …)" notice.
		leftParts = append(leftParts, fmt.Sprintf("◷ %d queued", n))
	}
	for _, sa := range m.subAgents {
		leftParts = append(leftParts, "◇ "+sa.Name)
	}
	// Cron wakeups + silent fires (2026-05-13). The wakeup chip
	// makes ScheduleWakeup self-documenting — without it the user
	// has no UI hint that the agent set itself a future trigger.
	// The silent-fires badge mirrors hermes' SILENT_MARKER: silent
	// jobs land in audit logs, badge counts last-24h fires so the
	// user notices when a counter sticks at zero (= broken job).
	if chip := wakeupChip(); chip != "" {
		leftParts = append(leftParts, chip)
	}
	if chip := silentFiresChip(); chip != "" {
		leftParts = append(leftParts, chip)
	}
	if voiceActive() {
		leftParts = append(leftParts, "● rec")
	}
	if addr := bridgeCurrentAddr(); addr != "" {
		leftParts = append(leftParts, "↹ "+addr)
	}
	// Git branch (claude-code parity): shown when cwd is a repo so the
	// user sees at a glance which branch their edits will land on.
	// Cached via internal/tui/footer_indicators.go's per-frame snapshot;
	// no shell-out per frame.
	if branch := cachedGitBranch(); branch != "" {
		leftParts = append(leftParts, "⎇ "+branch)
	}
	// Short session id — handy when /sessions list is open and the user
	// wants to confirm "this is the one I'm in". Six chars is a
	// claude-code style: long enough to be unique, short enough to not
	// dominate the status line.
	if sid := m.sessionID; len(sid) >= 6 {
		leftParts = append(leftParts, "❉ "+sid[:6])
	}
	// (cwd badge intentionally NOT shown in the status bar — the user
	// already sees it in the welcome banner; duplicating it on every
	// frame just visually clutters the bottom row.)
	left := "  " + strings.Join(leftParts, " · ")

	publishBridgeSnapshot(m)

	// Right side: **context-window load** for the most recent API call
	// — input + cache (no output), as a percentage of the model's max
	// context. Mirrors claude-code's statusline `used_percentage`
	// (https://code.claude.com/docs/en/statusline.md): numerator is
	// `input_tokens + cache_creation_input_tokens + cache_read_input_tokens`,
	// denominator is `context_window_size`.
	//
	// Distinct from the spinner row's "↓ N tokens" (LastTotal — input+
	// output, per-turn cost) and from /cost (session-cumulative billing).
	// Three different numbers serving three different questions:
	//   spinner    → "what did the just-finished turn cost?"
	//   right side → "how full is my context window?"
	//   /cost      → "what have I billed this session?"
	//
	// Raw integer (not k-abbreviated) so the user sees every increment;
	// matches claude-code's statusline rendering.
	var right string
	used := m.totalTokens.ContextUsage()
	if used == 0 {
		// Fallback when the most-recent-API-call counters are still
		// zero (very first turn before the first usage event lands).
		// Use session-cumulative input as a placeholder so the right
		// side isn't completely blank during the cold-start window.
		// Feedback 2026-05-05: "the token count sometimes doesn't
		// show during running". Better to show an approximate
		// (cumulative-so-far) than to flicker from blank → number.
		used = m.totalTokens.in + m.totalTokens.cacheCreate + m.totalTokens.cacheRead
	}
	// History-bytes floor: take the larger of (API-reported context
	// usage, on-disk history estimate). Without this, a provider that
	// under-reports cache_read_input_tokens (some Anthropic-compat
	// gateways — MiniMax does this inconsistently) makes the
	// displayed number SWING DOWN between turns, which looks like
	// "context shrank" to users expecting monotone growth.
	//
	// estimateTokens counts every serialized byte in Loop.Messages so
	// it only drops when real compaction fires — and compaction ALSO
	// emits a "[info] context snipped: ~N → ~M tokens" event, so a
	// drop without that event is the signal of a provider bug, not a
	// metis-side context change. User reported:
	//   "第3轮 5w token, 第4轮 3w, 还降低了" — this fixes that.
	if m.loop != nil {
		if est := m.loop.EstimateContextTokens(); est > used {
			used = est
		}
	}
	if used > 0 {
		right = formatTokensRaw(used) + " tokens"
		if m.loop != nil && m.loop.Provider != nil {
			if cap := m.loop.Provider.MaxContextTokens(); cap > 0 {
				right = fmt.Sprintf("%s (%s)", right, formatContextPct(used, cap))
			}
		}
		// Cost estimate (claude-code's statusline shows a $ figure).
		// Best-effort: catalog price × tokens. Skipped silently when
		// catalog has no entry for this model id, so unknown / custom
		// providers don't show a misleading $0.00.
		if cost := estimateCost(m); cost > 0 {
			right = fmt.Sprintf("$%.4f · %s", cost, right)
		}
	}

	w := m.width
	if w <= 0 {
		w = 80
	}
	gap := w - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}

	s.WriteString("\n")
	s.WriteString(styleMuted.Render(left))
	s.WriteString(strings.Repeat(" ", gap))
	if right != "" {
		// Token total is information-dense — render in primary text
		// rather than muted so the eye lands on it without effort.
		// claude-code shows the count in default-foreground; metis used
		// to mute it which hid the most useful number on screen.
		s.WriteString(styleText.Render(right))
	}
	s.WriteString("\n")

	// Second status row: version on the right, mirroring claude-code's
	// "current: X · latest: Y" hint. We only know `current` reliably
	// (this binary's build-time version); `latest` shows when a cached
	// check has been written to ~/.metis/latest_version. Absent that
	// file we just print `current`, which is still useful for "did I
	// install the build I just compiled?" sanity.
	verRight := renderVersionLine()
	if verRight != "" {
		vGap := w - lipgloss.Width(verRight) - 2
		if vGap < 1 {
			vGap = 1
		}
		s.WriteString(strings.Repeat(" ", vGap))
		s.WriteString(styleMuted.Render(verRight))
		s.WriteString("\n")
	}

	if user := statusLineCurrent(); user != "" {
		if lipgloss.Width(user) > w-4 {
			user = truncateRunes(user, w-4)
		}
		s.WriteString(styleMuted.Render("  " + user))
		s.WriteString("\n")
	}

	runStatusLineScript(m.model, string(m.gate.Mode()), m.sessionID, m.totalTokens.Total())

	return s.String()
}

// renderVersionLine builds the "current: vX.Y.Z · latest: vA.B.C" hint
// for the second status row. The latest-version side is best-effort —
// we read it from ~/.metis/latest_version (a file the user or a future
// background fetcher writes); when absent or equal to current we drop
// it so we don't print noise like "current: 0.1.0 · latest: 0.1.0".
func renderVersionLine() string {
	cur := version.Short()
	if cur == "" || cur == "unknown" {
		return ""
	}
	latest := readLatestVersion()
	latest = strings.TrimPrefix(latest, "v")
	if i := strings.Index(latest, "-"); i >= 0 {
		latest = latest[:i]
	}
	if latest == "" || latest == cur {
		return "current: v" + cur
	}
	return "current: v" + cur + " · latest: v" + latest
}

// readLatestVersion reads ~/.metis/latest_version (a single line) if it
// exists. We deliberately don't reach out to the network here: the chat
// surface is the wrong place to do a blocking HTTP call, and metis is a
// local-only dev tool — a separate updater can write this file when/if
// we add one. Returns "" on any error so the caller can fall back to
// just printing the current version.
func readLatestVersion() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(home + "/.metis/latest_version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// effortGlyph maps the model's reasoning-effort dial to a circle-fill
// glyph that telegraphs intensity.
func effortGlyph(e llm.Effort) string {
	switch e {
	case llm.EffortLow:
		return "○"
	case llm.EffortMedium:
		return "◐"
	case llm.EffortHigh:
		return "●"
	}
	return ""
}

// formatTokens prints abbreviated token counts (k/M suffix) — used by
// the spinner row where horizontal space is tight and per-turn precision
// isn't required ("↓ 38k tokens" mid-turn is fine; the user is watching
// the spinner anyway).
//
// 0–999 → "847", 1000–9999 → "3.1k", 10000+ → "12k", ≥1M → "1.2M".
func formatTokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 10000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	case n < 1000000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
}

// formatTokensRaw prints the exact integer count (no abbreviation). Used
// by the bottom-right status bar where the user wants to see every
// token tick — matches claude-code's statusline rendering, which shows
// the raw number rather than rounding to "k".
func formatTokensRaw(n int) string {
	return fmt.Sprintf("%d", n)
}

// modeIcon picks a vim-style mode glyph + color (claude-code's status bar
// style). Each mode gets a glyph that telegraphs its semantics.
//
// v2: lipgloss.Color became a function returning color.Color, so the
// return type is now image/color.Color.
func modeIcon(mode string) (glyph string, c color.Color) {
	switch mode {
	case "acceptEdits":
		return "⏵⏵", lipgloss.Color("#64b5f6") // claude-code's autoAccept (PermissionMode.ts:62)
	case "bypass":
		// claude-code's bypassPermissions uses ⏵⏵ in the `error` color
		// family (PermissionMode.ts:69) — same glyph as acceptEdits but
		// red to telegraph "this one is dangerous". Matches the source
		// exactly now that we removed the old ⏩ stand-in.
		return "⏵⏵", lipgloss.Color("#e57373")
	case "plan":
		return "⏸ ", lipgloss.Color("#81c784")
	case "deny":
		return "⏹ ", lipgloss.Color("#e57373")
	case "ask":
		// Match claude-code's `default` mode (PermissionMode.ts:48):
		// symbol = '' (empty). The most conservative mode renders
		// without a badge so its presence doesn't visually compete with
		// the more permissive modes the user explicitly cycled into.
		return "", lipgloss.Color("#a0a0a0")
	}
	return "", lipgloss.Color("#a0a0a0")
}

// renderHints draws the bottom-of-screen mode indicator.
//
// For `ask` mode (default) we omit the bold "ask mode" badge entirely
// and show only a dim "shift+tab to cycle" hint — matches claude-code's
// PermissionMode.ts:45-51 where `default` carries an empty symbol so
// the most conservative state renders without visual emphasis.
// Other modes keep the badge: the user explicitly cycled into them, so
// telegraphing "you're not in default anymore" is the right call.
func renderHints(m *Model) string {
	if m.permActive {
		return ""
	}
	mode := string(m.gate.Mode())
	glyph, col := modeIcon(mode)

	var s strings.Builder
	s.WriteString("  ")
	if glyph == "" {
		// `ask` mode — quiet. Just the discovery hint so the user
		// knows Shift+Tab is available, no badge.
		s.WriteString(styleMuted.Render("shift+tab to cycle modes"))
	} else {
		modeStyle := lipgloss.NewStyle().Foreground(col).Bold(true)
		s.WriteString(modeStyle.Render(glyph + " " + mode + " mode"))
		s.WriteString(styleMuted.Render(" on (shift+tab to cycle)"))
	}
	if m.showPalette {
		s.WriteString(styleMuted.Render(" · ↑↓/Tab to navigate · Esc to close"))
	}
	s.WriteString("\n")
	return s.String()
}
