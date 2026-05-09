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
	s.WriteString(styleAccent.Render("  " + frame + " "))
	if m.spinnerSub != "" {
		s.WriteString(toolUseFlashStyle(elapsed).Render(m.spinnerVerb))
		s.WriteString(styleDim.Render(" · " + truncate(m.spinnerSub, 35)))
	} else {
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
	if used > 0 {
		right = formatTokensRaw(used) + " tokens"
		if m.loop != nil && m.loop.Provider != nil {
			if cap := m.loop.Provider.MaxContextTokens(); cap > 0 {
				pct := used * 100 / cap
				right = fmt.Sprintf("%s (%d%%)", right, pct)
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
	case "auto":
		return "▶▶", lipgloss.Color("#64b5f6")
	case "bypass":
		return "⏩", lipgloss.Color("#ffb74d")
	case "plan":
		return "⏸ ", lipgloss.Color("#81c784")
	case "deny":
		return "⏹ ", lipgloss.Color("#e57373")
	case "ask":
		return "▶ ", lipgloss.Color("#a0a0a0")
	}
	return "▶ ", lipgloss.Color("#a0a0a0")
}

// renderHints draws the bottom-of-screen mode indicator.
func renderHints(m *Model) string {
	if m.permActive {
		return ""
	}
	mode := string(m.gate.Mode())
	glyph, col := modeIcon(mode)
	modeStyle := lipgloss.NewStyle().Foreground(col).Bold(true)

	var s strings.Builder
	s.WriteString("  ")
	s.WriteString(modeStyle.Render(glyph + " " + mode + " mode"))
	s.WriteString(styleMuted.Render(" on (shift+tab to cycle)"))
	if m.showPalette {
		s.WriteString(styleMuted.Render(" · ↑↓/Tab to navigate · Esc to close"))
	}
	s.WriteString("\n")
	return s.String()
}
