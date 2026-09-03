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
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/Ricardo-M-L/metis/internal/llm"
	tuilist "github.com/Ricardo-M-L/metis/internal/tui/list"
	"github.com/Ricardo-M-L/metis/internal/version"
)

// renderSubAgentChip formats one sub-agent as a compact status-bar
// pill. claude-code's Task framework renders a row per running task
// with the latest tool name + an elapsed-seconds timer; metis stays
// single-row by squeezing the same fields into one chip.
//
// Format (longest first; each segment elided when its data is missing):
//
//	◇ alice · Read · 23s · 7t   (running)
//	✓ alice · 47s · 12t         (completed, lingers for subAgentLingerDuration)
//	✗ alice · 3s                (failed, lingers)
//
// LastTool is omitted in the terminal-state forms because it's stale
// the moment Status flipped. Length cap kept loose — the status bar
// wraps when chips overflow.
func renderSubAgentChip(sa SubAgentInfo) string {
	glyph := "◇"
	includeLastTool := true
	switch sa.Status {
	case "completed":
		glyph = "✓"
		includeLastTool = false
	case "failed":
		glyph = "✗"
		includeLastTool = false
	}
	parts := []string{glyph + " " + sa.Name}
	if includeLastTool && sa.LastTool != "" {
		parts = append(parts, sa.LastTool)
	}
	if !sa.StartedAt.IsZero() {
		// For finished pills, freeze elapsed at FinishedAt so the
		// chip's last reading is the actual run duration rather than
		// "23s, 24s, 25s..." ticking on after completion.
		end := time.Now()
		if !sa.FinishedAt.IsZero() {
			end = sa.FinishedAt
		}
		elapsed := end.Sub(sa.StartedAt)
		parts = append(parts, formatSubAgentElapsed(elapsed))
	}
	if sa.ToolsCount > 0 {
		parts = append(parts, fmt.Sprintf("%dt", sa.ToolsCount))
	}
	return strings.Join(parts, " · ")
}

// formatSubAgentElapsed picks a compact unit:
//
//	<60s → 23s
//	<60m → 4m
//	else  → 2h
//
// Doesn't try to be precise — the chip is a "still alive" signal,
// not a stopwatch.
func formatSubAgentElapsed(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

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
	m.rebuildInputSelectionSurface()
	bodyLines := strings.Split(body, "\n")
	m.inputBodyHeight = len(bodyLines)
	// Two cells of outer chrome plus textarea's two-cell "> " / "  "
	// prompt. Mouse coordinates at or after this X belong to source text.
	m.inputContentStartX = 4
	// chatView clamps every final frame row to termW-1 cells. Keep hit-testing
	// inside that same visible half-open range on very narrow terminals.
	m.inputContentEndX = min(m.inputContentStartX+m.input.Width(), termW-1)
	selectionStart, selectionEnd, hasInputSelection := m.inputSelection.Range(m.inputSurface.value)
	for i := range bodyLines {
		globalRow := m.inputSurface.scrollOffset + i
		if hasInputSelection {
			lo, hi, ok := m.inputSelectionColumns(globalRow, selectionStart, selectionEnd)
			if !ok {
				continue
			}
			// bodyLines still includes textarea's two-cell prompt; convert
			// source-content columns to body-relative columns here. The outer
			// two-cell chrome prefix is added below and is intentionally not
			// selectable.
			bodyLines[i] = tuilist.HighlightColumns(bodyLines[i], 2+lo, 2+hi)
		}
	}
	body = strings.Join(bodyLines, "\n")

	// dividerW leaves a 1-cell gap on the right so the total visual width
	// is termW - 1, not termW. Many terminals (Terminal.app, iTerm2 default
	// "wrap at right margin", Wezterm) auto-wrap when the cursor advances
	// past the last column, which silently consumed an extra row per
	// divider — the visible symptom was the bottom divider clipped off
	// the screen (image #41 user feedback 2026-05-18: "之前双横线框只剩一条").
	// 2 leading + dividerW chars + 1 trailing gap = termW - 1 total.
	dividerW := termW - 3
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

// clampLine truncates a single rendered row (already styled, ANSI escapes
// included) to fit `width` terminal cells. The chat list measures item
// heights by counting newlines: a styled row longer than the viewport
// wraps in the TERMINAL (soft-wrap) but not in our count, so the
// renderer's frame-diff walks one row short on the next repaint and
// leaves stale fragments of the old frame on screen (2026-08-04 user
// screenshot: "g" / "1." debris on the left margin during Compacting,
// caused by the 74-cell "Compacting at auto window (235k tokens)"
// sub-line soft-wrapping on a ~72-col pane). Truncating here guarantees
// one logical line == one physical row.
//
// xansi.Truncate is ANSI-aware (never cuts an escape sequence) and
// cell-aware (CJK glyphs count as 2 cells), and appends an ellipsis
// when it actually truncates.
func clampLine(line string, width int) string {
	if width <= 0 {
		width = 80
	}
	if xansi.StringWidth(line) <= width {
		return line
	}
	return xansi.Truncate(line, width, "…")
}

// clampBlock applies clampLine to every newline-separated row of a
// rendered block. Trailing-newline structure is preserved because we
// split and rejoin on the same separator.
func clampBlock(block string, width int) string {
	nl := "\n"
	if !strings.Contains(block, nl) {
		return clampLine(block, width)
	}
	rows := strings.Split(block, nl)
	for i, r := range rows {
		rows[i] = clampLine(r, width)
	}
	return strings.Join(rows, nl)
}

// renderSpinnerStatus builds the streaming-state line:
//
//   - Verb sub (12s · ↓ 3.1k tokens · thought for 1s)
//
// All bracketed parts are conditional. The leading "Verb" is shimmered
// from 3s onward so quick replies don't flicker the dimming animation.
func renderSpinnerStatus(m *Model) string {
	// A compaction is a nested activity within a (potentially very long) turn.
	// Its clock, animation phase and token counter must all start at
	// EventCompactionStart; otherwise a turn that began hours ago renders a new
	// compaction as `9h 16m` and carries the previous request's token count into
	// the compaction row. This is display-state isolation only: the normal turn
	// clock remains untouched and resumes after EventCompactionEnd.
	compacting := strings.HasPrefix(m.spinnerOverride, "Compacting conversation")
	activityStartedAt := m.spinnerStartedAt
	if compacting && !m.compactionStartedAt.IsZero() {
		activityStartedAt = m.compactionStartedAt
	}
	elapsed := time.Duration(0)
	if !activityStartedAt.IsZero() {
		elapsed = time.Since(activityStartedAt)
	}
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
	if !compacting && m.spinnerSub != "" {
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
	if compacting {
		// EventCompactionProgress carries cumulative UTF-8 output bytes, not
		// provider token usage. Surface a clearly approximate summary-output
		// count instead of leaking LastIn/LastOut from the parent turn.
		if summaryTokens := m.spinnerCompactionBytes / 4; summaryTokens > 0 {
			parts = append(parts, fmt.Sprintf("↓ ≈%s summary tokens", formatTokens(summaryTokens)))
		}
	} else {
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
	// 2026-08-04 line-wrap fix: assemble the spinner main line into a
	// local buffer first, then clamp to the terminal width before it
	// hits the chat list. The list measures item heights by counting
	// newlines, so a spinner row wider than the viewport soft-wraps in
	// the terminal but not in our count — the next frame's diff then
	// walks one row short and leaves stale debris on screen (the "g" /
	// "1." fragments in the user's screenshot, which came from this
	// exact row during Compacting).
	var mainRow strings.Builder
	mainRow.WriteString(styleAccent.Render("  " + frame + " "))
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
		mainRow.WriteString(shimmerStyle(elapsed).Render(m.spinnerOverride))
	case m.spinnerSub != "":
		mainRow.WriteString(toolUseFlashStyle(elapsed).Render(m.spinnerVerb))
		mainRow.WriteString(styleDim.Render(" · " + truncate(m.spinnerSub, 35)))
	default:
		mainRow.WriteString(shimmerStyle(elapsed).Render(m.spinnerVerb))
	}
	if len(parts) > 0 {
		// Status info (elapsed · tokens · thought) — readable in dim,
		// not buried in muted grey. Parens themselves stay muted as
		// punctuation so the eye reads through them.
		mainRow.WriteString(styleMuted.Render(" ("))
		mainRow.WriteString(styleDim.Render(strings.Join(parts, " · ")))
		mainRow.WriteString(styleMuted.Render(")"))
	}
	s.WriteString(clampLine(mainRow.String(), m.width))
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
//	▰▰▰▰▰▰▰▰▰▱▱▱▱▱▱▱▱▱▱▱▱▱
//	└ Compacting at auto window (170k tokens) · /autocompact to configure
//
// The progress is a time-driven, left-to-right visual estimate. The
// compactor reports cumulative output bytes but not the final summary
// size, so those bytes cannot honestly be converted to a percentage.
// Instead the bar advances monotonically, then stops with two cells
// deliberately empty until EventCompactionEnd clears the row. This
// gives users a progressive animation without implying that we know
// how much work remains. Layout mirrors claude-code's compaction display
// (image #19 user feedback 2026-05-15).
func renderCompactionExtras(m *Model) string {
	const barWidth = 22
	startedAt := m.compactionStartedAt
	if startedAt.IsZero() {
		// Cold-render path (tests, pre-EventCompactionStart): fall back
		// to spinnerStartedAt so the bar still advances in test fixtures.
		startedAt = m.spinnerStartedAt
	}
	elapsed := time.Duration(0)
	if !startedAt.IsZero() {
		elapsed = time.Since(startedAt)
	}
	filled := compactionProgressCells(elapsed, barWidth)

	var bar strings.Builder
	for i := 0; i < barWidth; i++ {
		if i < filled {
			bar.WriteString("▰")
		} else {
			bar.WriteString("▱")
		}
	}

	// Use the same authoritative boundary as ShouldCompact. In production the
	// effective input cap reserves the provider's response budget, so copying
	// Threshold × MaxContextTokens here can advertise a later, unsafe trigger.
	// Fall back to 85% of the provider cap only for legacy callers/tests that
	// have no Compactor.
	autoWindow := 0
	if m.loop != nil {
		if m.loop.Compactor != nil {
			autoWindow = m.loop.Compactor.TriggerTokens()
		} else if m.loop.Provider != nil {
			if cap := m.loop.Provider.MaxContextTokens(); cap > 0 {
				autoWindow = int(float64(cap) * 0.85)
			}
		}
	}

	var s strings.Builder
	s.WriteString("  ")
	s.WriteString(styleAccent.Render(bar.String()))
	s.WriteString("\n")
	// 2026-08-04: clamp the sub-line to the terminal width. The full
	// string is ~74 cells ("└ Compacting at auto window (235k tokens) ·
	// /autocompact to configure") and soft-wrapped on panes narrower
	// than that, desyncing the renderer's per-frame line accounting —
	// the debris in the user's screenshot came from this row.
	if autoWindow > 0 {
		s.WriteString(clampLine("  "+styleDim.Render(fmt.Sprintf("└ Compacting at auto window (%s tokens) · /autocompact to configure", formatTokens(autoWindow))), m.width))
	} else {
		s.WriteString(clampLine("  "+styleDim.Render("└ Compacting at auto window · /autocompact to configure"), m.width))
	}
	s.WriteString("\n")
	return s.String()
}

// compactionProgressCells maps elapsed wall time to a monotonic visual
// estimate. It deliberately caps at width-2 cells after eight seconds:
// the remaining cells mean "still waiting for the real completion
// event", not a fabricated 100%. A quadratic ease-out makes short,
// typical compactions visibly advance while longer calls slow into a
// stable waiting state instead of looping back and forth.
func compactionProgressCells(elapsed time.Duration, width int) int {
	if width <= 0 {
		return 0
	}
	if width <= 2 {
		return 1
	}
	if elapsed < 0 {
		elapsed = 0
	}

	const fillDuration = 8 * time.Second
	maxFilled := width - 2
	if elapsed >= fillDuration {
		return maxFilled
	}

	ratio := float64(elapsed) / float64(fillDuration)
	eased := 1 - (1-ratio)*(1-ratio)
	filled := 1 + int(eased*float64(maxFilled-1))
	if filled > maxFilled {
		return maxFilled
	}
	return filled
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
		effort := m.loop.EffortValue()
		if g := effortGlyph(effort); g != "" {
			leftParts = append(leftParts, g+" "+string(effort))
		}
		if m.loop.FastEnabled() {
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
		leftParts = append(leftParts, renderSubAgentChip(sa))
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

	// Right side: active context-window load. The loop anchors it to the latest
	// successful response's disjoint input/cache/output usage, then estimates
	// only local tool/user messages appended after that response. This follows
	// Codex/Claude's active-snapshot model instead of session-cumulative spend.
	//
	// Distinct from the spinner row's "↓ N tokens" (the latest call's cost)
	// and from /cost (session-cumulative billing).
	// Three different numbers serving three different questions:
	//   spinner    → "what did the just-finished turn cost?"
	//   right side → "how full is my context window?"
	//   /cost      → "what have I billed this session?"
	//
	// Raw integer (not k-abbreviated) so the user sees every increment;
	// matches claude-code's statusline rendering.
	var right string
	used := m.totalTokens.ContextUsage()
	// Loop owns the canonical active-context value: a validated latest usage
	// snapshot plus only the messages appended after it. Do not max this with
	// raw tokenTracker data. A malformed compatibility gateway may report an
	// impossible cache count; the loop deliberately rejects it, and letting the
	// raw tracker win here would resurrect the >100% bug in the status bar.
	if m.loop != nil {
		used = m.loop.EstimateContextTokens()
	}
	if used > 0 {
		right = formatTokensRaw(used) + " tokens"
		if m.loop != nil {
			// ContextWindow is part of the loop's provider/model runtime
			// snapshot and is refreshed atomically on every session/model switch.
			// Do not re-query the provider while painting an idle frame: besides
			// wasting work, a catalog-backed implementation could observe a
			// different route than the one bound to this session.
			if _, _, cap := m.loop.ProviderModelSnapshot(); cap > 0 {
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
	// 2026-08-04: when many status chips are active (sub-agents, cron,
	// voice, bridge, branch, session id, todos, jobs, queue…) the left
	// side alone can exceed the terminal width. The old code clamped
	// `gap` at 1 but still emitted the full left string, so the row
	// soft-wrapped and desynced the frame-diff line accounting (same
	// root cause as the Compacting debris in the user screenshot).
	// Reserve space for the right side, then truncate the left to fit.
	rightW := lipgloss.Width(right)
	maxLeft := w - rightW - 3 // 1 leading gap cell + 2 trailing cells
	if maxLeft < 10 {
		maxLeft = 10
	}
	if lipgloss.Width(left) > maxLeft {
		left = xansi.Truncate(left, maxLeft, "…")
	}
	gap := w - lipgloss.Width(left) - rightW - 2
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
//
// Note: we deliberately don't filter "latest older than current" here.
// That case is handled upstream in internal/update/check.go MaybeCheck
// via the stale-cache invalidation: when the running binary is newer
// than the cached LatestTag, LastCheck is cleared so the next startup
// fetches fresh. Doing the filter here would hide the symptom (stale
// cache) without fixing the cause — exactly the wrong layer to
// intervene (2026-07-26 discussion).
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
	case "bypassPermissions":
		// claude-code's bypassPermissions uses ⏵⏵ in the `error` color
		// family (PermissionMode.ts:69) — same glyph as acceptEdits but
		// red to telegraph "this one is dangerous". Matches the source
		// exactly now that we removed the old ⏩ stand-in.
		return "⏵⏵", lipgloss.Color("#e57373")
	case "fullAccess":
		return "!!", lipgloss.Color("#ff5252")
	case "plan":
		return "⏸ ", lipgloss.Color("#81c784")
	case "dontAsk":
		// Claude Code renders dontAsk with the same double-chevron/error
		// treatment as bypassPermissions (PermissionMode.ts:73-78).
		return "⏵⏵", lipgloss.Color("#e57373")
	case "default":
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
// For `default` mode we omit the bold mode badge entirely
// and show only a dim "shift+tab to cycle" hint — matches claude-code's
// PermissionMode.ts:45-51 where `default` carries an empty symbol so
// the most conservative state renders without visual emphasis.
// Other modes keep the badge: the user explicitly cycled into them, so
// telegraphing "you're not in default anymore" is the right call.
func renderHints(m *Model) string {
	if m.permActive {
		// Keep a visible anchor under the input box while the permission
		// prompt owns the decision keys. Previously this returned "" so
		// the band collapsed — combined with the still-editable textarea
		// (keybind_permission routes letter keys through), users typed,
		// saw the mode badge vanish, and read the screen as "frozen".
		return styleWarn.Render("  ⏳ waiting for your permission decision above") +
			styleMuted.Render(" · you can keep typing here") + "\n"
	}
	if m.permissionModePending {
		return styleWarn.Render("  ⏳ permission mode → "+string(m.permissionModeTarget)) +
			styleMuted.Render(" · waiting for the current tool boundary") + "\n"
	}
	mode := string(m.gate.Mode())
	glyph, col := modeIcon(mode)

	var s strings.Builder
	s.WriteString("  ")
	if glyph == "" {
		// `default` mode — quiet. Just the discovery hint so the user
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

// renderQueuedPreview shows the messages the user has typed while a
// turn was in flight. Empty when the queue is empty.
//
// Why a dedicated band (not just the status-bar chip): prior to this
// the only feedback for `m.queuedPrompts` was the compact `◷ N queued`
// chip way down in the status bar. Users (2026-05-20 feedback) saw
// the input clear after Enter and assumed the message had been
// dropped — claude-code's PromptInputQueuedCommands.tsx renders each
// queued message as a faded message bubble between the input and
// status bar, which the user immediately recognises as "captured,
// will run next". We mirror that here with a one-line-per-message
// preview directly below renderHints so the eye lands on it without
// scanning the status bar.
//
// Display rules borrowed from claude-code:
//   - Cap at queuedPreviewMaxRows visible rows (3); collapse the rest
//     into a "+ N more queued" footer.
//   - Truncate each line at queuedPreviewMaxRunes (90 runes, rune-
//     aware to avoid CJK mid-codepoint corruption — same lesson as
//     toolArgsPreview).
//   - styleDim so the preview reads as anticipated future input, not
//     part of the live transcript.
func renderQueuedPreview(m *Model) string {
	if len(m.queuedPrompts) == 0 {
		return ""
	}
	const (
		queuedPreviewMaxRows  = 3
		queuedPreviewMaxRunes = 90
	)
	var b strings.Builder
	visible := m.queuedPrompts
	overflow := 0
	if len(visible) > queuedPreviewMaxRows {
		overflow = len(visible) - queuedPreviewMaxRows
		visible = visible[:queuedPreviewMaxRows]
	}
	for _, q := range visible {
		// Collapse newlines so a pasted multi-line prompt still fits
		// one preview row. The full text survives in m.queuedPrompts
		// and runs verbatim when its turn comes.
		line := strings.ReplaceAll(q.Text, "\n", " ⏎ ")
		rs := []rune(line)
		if len(rs) > queuedPreviewMaxRunes {
			line = string(rs[:queuedPreviewMaxRunes-1]) + "…"
		}
		b.WriteString("  ")
		// Priority badge: Now → "!" Later → ".". Default Next has
		// no badge so the common case stays as terse as the
		// pre-priority preview. Use effective() so uninitialised
		// (zero) Priority renders as Next without a badge.
		badge := ""
		switch q.Priority.effective() {
		case QueuePriorityNow:
			badge = "! "
		case QueuePriorityLater:
			badge = ". "
		}
		b.WriteString(styleDim.Render("⏵ queued · " + badge + line))
		b.WriteString("\n")
	}
	if overflow > 0 {
		b.WriteString("  ")
		b.WriteString(styleMuted.Render(fmt.Sprintf("+ %d more queued (press Esc to cancel the running turn first to send any sooner)", overflow)))
		b.WriteString("\n")
	}
	return b.String()
}
