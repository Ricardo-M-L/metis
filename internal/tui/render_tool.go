package tui

// render_tool.go — tool-call rendering. Each ToolEvent prints as a
// claude-code-style two-row block:
//
//	⏺ ToolName(arg)
//	  ⎿ 42ms ✓ summary line
//	         (optional preview body — diff for Edit, first lines for Bash, …)
//
// Per-tool dispatch lives here. Edit/Write get structured diffs;
// everything else gets a line-capped output preview.

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	udiff "github.com/aymanbagabas/go-udiff"

	"github.com/Ricardo-M-L/metis/internal/themes"
)

// renderToolEvent prints one tool call. Leader row repeats whether
// we're mid-flight or done; for in-flight calls we end the leader with
// " …" and skip the result row entirely. Each tool gets its own summary
// phrasing via summarizeToolResult so the transcript reads
// "Read foo.py (350 lines)" instead of dumping 350 lines.
//
// expanded controls per-call truncation policy: when true (user hit
// ctrl+O), Edit diffs / Bash output / error bodies render in full
// instead of capping at 5-20 lines.
func renderToolEvent(te ToolEvent, expanded bool) string {
	var s strings.Builder

	// Leader row: ⏺ toolname(arg)
	//
	// Tool name is lowercased for display only — the registry name (what
	// the LLM sees over the wire) stays PascalCase to match claude-code-
	// trained model expectations. claude-code's TUI renders tool names
	// in their original case but visually they're rare-cased so the
	// effect reads similar; metis goes one further and just lowercases
	// at the render boundary because the user reported uppercase looks
	// "loud" against the soft-tone TUI palette.
	// Leader color follows claude-code's ToolUseLoader pattern: in-flight
	// rows use the accent (blue/orange) "tool starting" color; completed
	// rows pop bright **green** (success) or **red** (error) so the
	// transcript at-a-glance shows you which calls succeeded. Earlier
	// metis released with completed rows muted to #606060 grey, which
	// the user reported as "too washed out" vs claude-code in the same
	// terminal — this fix swaps the muted-once-finished branch for
	// success/error so the bullet carries actual semantic color.
	leaderColor := styleAccent
	if te.Kind != "start" {
		if te.IsError {
			leaderColor = styleErr
		} else {
			leaderColor = styleSuccess
		}
	}
	s.WriteString(leaderColor.Render("  " + glyphBullet + " "))
	s.WriteString(styleToolName.Render(displayToolName(te.ToolName)))
	if args := toolArgsPreview(te.ToolName, te.Input); args != "" {
		s.WriteString(styleMuted.Render("("))
		// WebFetch: wrap the truncated URL display with OSC 8 so
		// terminals that support it (iTerm2, WezTerm, Alacritty,
		// GNOME, Windows Terminal) make the URL clickable. Other
		// terminals see the same text and just no-op the escape.
		if te.ToolName == "WebFetch" {
			if url, ok := te.Input["url"].(string); ok && url != "" {
				s.WriteString(osc8Link(styleMuted.Render(args), url))
			} else {
				s.WriteString(styleMuted.Render(args))
			}
		} else {
			s.WriteString(styleMuted.Render(args))
		}
		s.WriteString(styleMuted.Render(")"))
	}
	if te.Kind == "start" {
		s.WriteString(styleMuted.Render(" …"))
		s.WriteString("\n")
		return s.String()
	}
	s.WriteString("\n")

	// Result row: ⎿ Xms ✓/✗ summary
	// The leaf glyph stays muted (it's just a connector), but the
	// summary itself ("0.5s · Read foo.py (350 lines)") is information
	// dense — show it in the dim-but-readable secondary tone instead of
	// muted grey. The user flagged that too much of the screen renders
	// as low-contrast grey vs claude-code, and this row is one of the
	// most-read lines in the transcript.
	// Connector glyph stays subdued (it's structural, not informational)
	// but at #a0a0a0 (styleDim), not #606060 (styleMuted) — the latter
	// vanishes against most dark-theme backgrounds. claude-code uses
	// `dimColor` (ANSI Faint) for the same role, which renders ~50% of
	// the active foreground rather than a fixed ultra-grey.
	s.WriteString(styleDim.Render("    " + glyphTreeLeaf + "  "))
	if te.IsError {
		s.WriteString(styleErr.Render("✗ "))
	} else {
		s.WriteString(styleAccent.Render("✓ "))
	}
	s.WriteString(styleDim.Render(summarizeToolResult(te)))
	s.WriteString("\n")

	// Body: structured diff for Edit/Write, task-list for TodoWrite,
	// line-capped preview otherwise. On error, surface the error
	// output so the user can see WHY it failed.
	if te.IsError {
		if te.Output != "" {
			s.WriteString(renderErrorBody(te.Output, expanded))
		}
	} else {
		switch te.ToolName {
		case "Edit":
			s.WriteString(renderEditDiff(te.Input, expanded))
		case "Write":
			s.WriteString(renderWritePreview(te.Input, expanded))
		case "TodoWrite":
			s.WriteString(renderTodoSnapshot())
		default:
			if te.Output != "" {
				s.WriteString(renderToolOutputPreview(te.ToolName, te.Output, expanded))
			}
		}
	}
	s.WriteString("\n")
	return s.String()
}

// renderErrorBody is the error-path counterpart to renderToolOutputPreview:
// same 5-line cap with truncation tail, but rendered in error red so the
// failure mode is visually unmistakable.
//
// Long lines use MIDDLE truncation (`head … tail`) instead of left
// truncation. Errors like `stat /Users/.../foo/bar/index.ts/loop.go:
// no such file or directory` carry information at BOTH ends — the
// verb ("stat") at the front, the offending basename ("loop.go") and
// the system error tail at the back. Left-truncate hid the basename,
// confusingly making the error look unrelated to the read it came
// from. Image bug 2026-05-15.
func renderErrorBody(out string, expanded bool) string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return ""
	}
	errStyle := lipgloss.NewStyle().Foreground(accentRed)
	lines := strings.Split(out, "\n")
	const maxPreview = 5
	show := lines
	if !expanded && len(show) > maxPreview {
		show = show[:maxPreview]
	}
	var s strings.Builder
	for _, ln := range show {
		s.WriteString(errStyle.Render("       " + truncateMiddle(ln, 120)))
		s.WriteString("\n")
	}
	if !expanded && len(lines) > maxPreview {
		s.WriteString(styleMuted.Render(fmt.Sprintf("       … +%d more lines (ctrl+O to expand)", len(lines)-maxPreview)))
		s.WriteString("\n")
	}
	return s.String()
}

// truncateMiddle keeps the first and last segments of s when the rune
// count exceeds maxRunes, joining them with " … ". Bias is slightly
// toward the tail (sysprefix verbs are short — `stat ` / `open ` /
// `lstat ` — the meaningful content is at the back). For
//
//	stat /a/very/long/path/that/exceeds/the/cap/loop.go: no such file
//
// at maxRunes=60 you get
//
//	stat /a/very/long/path … cap/loop.go: no such file
//
// instead of the left-truncated form which hid `loop.go`.
func truncateMiddle(s string, maxRunes int) string {
	rs := []rune(s)
	if len(rs) <= maxRunes {
		return s
	}
	const sep = " … "
	keep := maxRunes - len(sep)
	if keep < 4 {
		// Too tight to do middle-truncate gracefully — fall back to
		// the original tail-cut form.
		return string(rs[:maxRunes-1]) + "…"
	}
	// 40% to head, 60% to tail (favor the more diagnostic end).
	head := keep * 2 / 5
	tail := keep - head
	return string(rs[:head]) + sep + string(rs[len(rs)-tail:])
}

// summarizeToolResult crafts the per-tool one-line description that
// follows the ⎿ checkmark. Format is `<elapsed> · <tool-specific phrase>`.
func summarizeToolResult(te ToolEvent) string {
	dur := te.Duration.Truncate(time.Millisecond).String()
	switch te.ToolName {
	case "Read":
		path := stringField(te.Input, "path", "file_path")
		// Error-path: te.Output is the error message (e.g. a stat
		// "no such file" line), so lineCount(Output) reports "(1 lines)"
		// which reads as a tiny successful read. Surface the failure
		// explicitly instead — image bug 2026-05-15.
		if te.IsError {
			if path != "" {
				return fmt.Sprintf("%s · Read %s — failed", dur, basename(path))
			}
			return fmt.Sprintf("%s · Read failed", dur)
		}
		n := lineCount(te.Output)
		if path != "" {
			return fmt.Sprintf("%s · Read %s (%d lines)", dur, basename(path), n)
		}
		return fmt.Sprintf("%s · Read %d lines", dur, n)
	case "Edit":
		added, removed := countEditDiff(te.Input)
		return fmt.Sprintf("%s · Added %d lines, removed %d lines", dur, added, removed)
	case "Write":
		path := stringField(te.Input, "path", "file_path")
		if c, ok := te.Input["content"].(string); ok {
			if path != "" {
				return fmt.Sprintf("%s · Wrote %s (%d lines)", dur, basename(path), lineCount(c))
			}
			return fmt.Sprintf("%s · Wrote %d lines", dur, lineCount(c))
		}
		return dur
	case "Bash":
		first := firstNonEmptyLine(te.Output)
		if first == "" {
			return dur
		}
		return fmt.Sprintf("%s · %s", dur, truncate(first, 60))
	case "Glob":
		if strings.HasPrefix(strings.TrimSpace(te.Output), "(no matches)") {
			return fmt.Sprintf("%s · No files matched", dur)
		}
		n := lineCount(te.Output)
		word := "files"
		if n == 1 {
			word = "file"
		}
		return fmt.Sprintf("%s · Found %d %s", dur, n, word)
	case "Grep":
		out := strings.TrimSpace(te.Output)
		if strings.HasPrefix(out, "(no matches)") {
			return fmt.Sprintf("%s · No matches", dur)
		}
		// Strip optional pagination footer ("[truncated at N matches…]")
		// before counting so the user sees the actual match count, not
		// match-count + footer line.
		body := out
		if idx := strings.LastIndex(body, "\n[truncated"); idx >= 0 {
			body = body[:idx]
		}
		n := lineCount(body)
		word := "matches"
		if n == 1 {
			word = "match"
		}
		return fmt.Sprintf("%s · %d %s", dur, n, word)
	default:
		out := strings.TrimSpace(te.Output)
		if out == "" {
			return dur
		}
		return fmt.Sprintf("%s · %s", dur, truncate(firstNonEmptyLine(out), 60))
	}
}

// renderEditDiff prints a colored unified diff between the Edit tool's
// old_string and new_string. claude-code's version: line-number gutter
// + green-bg `+` / red-bg `-` with `equal` rows providing 1 line of
// context. Capped at maxDiffLines so a 200-line replacement doesn't
// drown the chat surface.
const maxDiffLines = 20

func renderEditDiff(input map[string]any, expanded bool) string {
	// metis's Edit tool uses `old` / `new` (see internal/tools/builtin/
	// edit.go's InputSchema). claude-code-style external tools may pass
	// `old_string` / `new_string`. Read both — the actual Edit-tool
	// field name takes priority since that's what fires in 99% of
	// turns; the longer names cover any externally-defined tool.
	oldS, _ := input["old"].(string)
	newS, _ := input["new"].(string)
	if oldS == "" && newS == "" {
		oldS, _ = input["old_string"].(string)
		newS, _ = input["new_string"].(string)
	}
	if oldS == "" && newS == "" {
		return ""
	}
	edits := udiff.Strings(oldS, newS)
	if len(edits) == 0 {
		return ""
	}
	uout, err := udiff.ToUnifiedDiff("", "", oldS, edits, 1)
	if err != nil || len(uout.Hunks) == 0 {
		return ""
	}

	addBgOnly := lipgloss.NewStyle().Background(themes.Current().DiffAddBg)
	delBgOnly := lipgloss.NewStyle().Background(themes.Current().DiffDelBg)
	addStrong := lipgloss.NewStyle().Background(themes.Current().DiffAddBg).Foreground(themes.Current().DiffAddFg).Bold(true)
	delStrong := lipgloss.NewStyle().Background(themes.Current().DiffDelBg).Foreground(themes.Current().DiffDelFg).Bold(true)
	gutterStyle := lipgloss.NewStyle().Foreground(textMuted)
	filename := pickLanguageFromInput(input)

	var s strings.Builder
	rendered := 0
	totalDiffLines := 0
	for _, h := range uout.Hunks {
		totalDiffLines += len(h.Lines)
	}

	limit := maxDiffLines
	if expanded {
		limit = 1 << 30
	}
	for _, h := range uout.Hunks {
		oldLine := h.FromLine
		newLine := h.ToLine
		i := 0
		for i < len(h.Lines) {
			if rendered >= limit {
				break
			}
			ln := h.Lines[i]
			content := truncateRunes(strings.TrimRight(ln.Content, "\n"), 100)

			// Paired Delete + Insert → word-level diff.
			var delMask, insMask []bool
			var partner *string
			if ln.Kind == udiff.Delete && i+1 < len(h.Lines) && h.Lines[i+1].Kind == udiff.Insert {
				partnerContent := truncateRunes(strings.TrimRight(h.Lines[i+1].Content, "\n"), 100)
				delMask, insMask = wordDiffMasks(content, partnerContent)
				partner = &partnerContent
			}

			switch ln.Kind {
			case udiff.Equal:
				s.WriteString(gutterStyle.Render(fmt.Sprintf("       %4d   ", oldLine)))
				s.WriteString(styleMuted.Render(content))
				oldLine++
				newLine++
				i++
			case udiff.Delete:
				s.WriteString(gutterStyle.Render(fmt.Sprintf("       %4d ", oldLine)))
				s.WriteString(delBgOnly.Render("- "))
				if delMask != nil {
					s.WriteString(applyMask(content, delMask,
						func(s string) string { return delBgOnly.Render(s) },
						func(s string) string { return delStrong.Render(s) }))
				} else {
					s.WriteString(delBgOnly.Render(highlightLine(content, filename)))
				}
				oldLine++
				i++
				if partner != nil {
					s.WriteString("\n")
					rendered++
					if rendered >= limit {
						break
					}
					s.WriteString(gutterStyle.Render(fmt.Sprintf("       %4d ", newLine)))
					s.WriteString(addBgOnly.Render("+ "))
					s.WriteString(applyMask(*partner, insMask,
						func(s string) string { return addBgOnly.Render(s) },
						func(s string) string { return addStrong.Render(s) }))
					newLine++
					i++
				}
			case udiff.Insert:
				s.WriteString(gutterStyle.Render(fmt.Sprintf("       %4d ", newLine)))
				s.WriteString(addBgOnly.Render("+ "))
				s.WriteString(addBgOnly.Render(highlightLine(content, filename)))
				newLine++
				i++
			}
			s.WriteString("\n")
			rendered++
		}
		if rendered >= limit {
			break
		}
	}

	if totalDiffLines > rendered {
		s.WriteString(styleMuted.Render(fmt.Sprintf("       … +%d more diff lines", totalDiffLines-rendered)))
		s.WriteString("\n")
	}
	return s.String()
}

// renderWritePreview shows the first lines of new file content with a
// green `+` gutter, since Write has no "before" to diff against.
func renderWritePreview(input map[string]any, expanded bool) string {
	c, _ := input["content"].(string)
	if c == "" {
		return ""
	}
	addBgOnly := lipgloss.NewStyle().Background(themes.Current().DiffAddBg)
	gutterStyle := lipgloss.NewStyle().Foreground(textMuted)
	filename := pickLanguageFromInput(input)

	lines := strings.Split(strings.TrimRight(c, "\n"), "\n")
	const maxShow = 10
	show := lines
	if !expanded && len(show) > maxShow {
		show = show[:maxShow]
	}
	var s strings.Builder
	for i, ln := range show {
		s.WriteString(gutterStyle.Render(fmt.Sprintf("       %4d ", i+1)))
		s.WriteString(addBgOnly.Render("+ "))
		s.WriteString(addBgOnly.Render(highlightLine(truncateRunes(ln, 100), filename)))
		s.WriteString("\n")
	}
	if !expanded && len(lines) > maxShow {
		s.WriteString(styleMuted.Render(fmt.Sprintf("       … +%d more lines (ctrl+O to expand)", len(lines)-maxShow)))
		s.WriteString("\n")
	}
	return s.String()
}

// renderToolOutputPreview shows the first ~5 lines of any tool output
// that doesn't have a custom renderer (Bash, Grep, WebFetch, …). Long
// runs get a `… +N more lines (ctrl+O to expand)` tail.
//
// Color strategy mirrors claude-code (image #23 user feedback 2026-05-16):
//
//   - **Code / match content** renders at the terminal's default
//     foreground (no dim). The actual *information* the user is here
//     to read.
//   - **Coordinate prefixes** — Read's `<lineno>\t` gutter, Grep's
//     `<path>:<line>:` prefix — get the styleDim (#a0a0a0) treatment.
//     Eye picks "where" out of "what" without the whole block fading
//     into a grey wash.
//
// Pre-fix every preview row was styleDim-wrapped end-to-end, which
// users reported as visually exhausting against a dark terminal: the
// content blurred into the chrome and you had to squint to find the
// match you ran the command for.
func renderToolOutputPreview(toolName, out string, expanded bool) string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	const maxPreview = 5
	show := lines
	if !expanded && len(show) > maxPreview {
		show = show[:maxPreview]
	}
	var s strings.Builder
	for _, ln := range show {
		s.WriteString("       ")
		s.WriteString(formatToolPreviewLine(toolName, truncateRunes(ln, 120)))
		s.WriteString("\n")
	}
	if !expanded && len(lines) > maxPreview {
		s.WriteString(styleMuted.Render(fmt.Sprintf("       … +%d more lines (ctrl+O to expand)", len(lines)-maxPreview)))
		s.WriteString("\n")
	}
	return s.String()
}

// formatToolPreviewLine applies per-tool prefix-dimming so the
// coordinate frame (line numbers / paths) renders dim while the
// payload renders at default fg. Tools we don't recognise fall
// through to default-fg for the whole line — safer than blanket-
// dimming, which is exactly the regression image #23 flagged.
func formatToolPreviewLine(toolName, ln string) string {
	switch toolName {
	case "Read":
		// cat -n format from Read tool: leading spaces + digits + tab + content.
		// Split on the FIRST tab — that's the gutter/content boundary
		// the Read tool itself emits (see internal/tools/builtin/read.go
		// "6-digit line number + tab + content"). When the format doesn't
		// match (e.g. a Read of a binary file fell through to a different
		// path), fall back to whole-line default-fg.
		if i := strings.IndexByte(ln, '\t'); i > 0 {
			return styleDim.Render(ln[:i+1]) + ln[i+1:]
		}
		return ln
	case "Grep", "Glob":
		// path:line:content (Grep) or just `path` (Glob). For Grep,
		// dim everything up to and including the second ':' separator;
		// the actual match content past that point gets default fg.
		// Glob has no inline content, so the whole line is "where" and
		// can stay dim.
		if toolName == "Glob" {
			return styleDim.Render(ln)
		}
		// Grep: find SECOND ':' to skip the line-number too.
		if i := strings.IndexByte(ln, ':'); i > 0 {
			if j := strings.IndexByte(ln[i+1:], ':'); j > 0 {
				cut := i + 1 + j + 1
				return styleDim.Render(ln[:cut]) + ln[cut:]
			}
			// Only one colon found (path:something) — dim the prefix.
			return styleDim.Render(ln[:i+1]) + ln[i+1:]
		}
		return ln
	default:
		// Bash output, WebFetch body, etc. — payload-only, no gutter
		// to dim. Default fg surfaces the content cleanly.
		return ln
	}
}

// renderTodoSnapshot is the body for a TodoWrite tool result — every
// task in the current session's list, rendered in claude-code's style:
// a tree-leaf glyph at the top, then one row per task with strikethrough
// for completed and bold for the in-progress one. Mirrors the image #57
// the user shared.
//
// We re-read the persisted task file (rather than parsing TodoWrite's
// own input) because a single TodoWrite call may only touch one row,
// but the visual the user expects is the WHOLE list. Reading from disk
// keeps the rendered snapshot consistent regardless of which row the
// LLM most recently touched.
func renderTodoSnapshot() string {
	sid := tasksCurrentSessionID()
	if sid == "" {
		return ""
	}
	items := tasksFullList(sid)
	if len(items) == 0 {
		return ""
	}
	return renderTaskItems(items)
}

// renderTaskItems is the pure presentation half of the snapshot — takes
// a slice of TaskItem, returns the formatted body. Split out so the
// test path can inject items without going through disk IO.
//
// Color/glyph mapping mirrors claude-code's TaskListV2.tsx getTaskIcon:
//
//	pending     → ◻ (squareSmall) + default text color
//	in_progress → ◼ (squareSmallFilled) + AccentOrange (claude-code's
//	              "claude" theme token #d77757)
//	completed   → ✔ (tick) + AccentGreen + strikethrough on the title
//
// Earlier metis painted all three glyphs in styleAccent (blue), which
// failed the at-a-glance "what's done / what's running" scan (image #1
// user comparison 2026-05-10).
func renderTaskItems(items []TaskItem) string {
	var s strings.Builder
	strike := lipgloss.NewStyle().Foreground(textSecondary).Strikethrough(true)
	current := lipgloss.NewStyle().Foreground(textPrimary).Bold(true)
	pending := lipgloss.NewStyle().Foreground(textSecondary)

	completedGlyph := lipgloss.NewStyle().Foreground(accentGreen)
	inProgressGlyph := lipgloss.NewStyle().Foreground(accentOrange)
	pendingGlyph := lipgloss.NewStyle().Foreground(textSecondary)

	for i, it := range items {
		// First row gets the L-leaf connector, subsequent rows just
		// indent. Mirrors claude-code's grouped-tool-result tree shape.
		prefix := "       "
		if i == 0 {
			prefix = "    " + glyphTreeLeaf + "  "
		}
		var glyph string
		var glyphStyle lipgloss.Style
		var styled string
		switch it.Status {
		case "completed":
			glyph = glyphTaskCompleted
			glyphStyle = completedGlyph
			styled = strike.Render(it.Content)
		case "in_progress":
			glyph = glyphTaskInProgress
			glyphStyle = inProgressGlyph
			styled = current.Render(it.Content)
		default: // pending or unknown
			glyph = glyphTaskPending
			glyphStyle = pendingGlyph
			styled = pending.Render(it.Content)
		}
		s.WriteString(styleMuted.Render(prefix))
		s.WriteString(glyphStyle.Render(glyph + " "))
		s.WriteString(styled)
		s.WriteString("\n")
	}
	return s.String()
}

// tasksCurrentSessionID reads the runtime-set current session id via
// the tasks package — the TodoWrite tool path uses the same accessor.
// Wrapped here so render_tool.go doesn't reach into internal/tasks
// directly (keeps the import surface narrow).
func tasksCurrentSessionID() string { return tasksCurrentSessionIDFn() }

// tasksCurrentSessionIDFn is overridable in tests so we can render the
// snapshot against a fixed task file without spinning up a runtime.
var tasksCurrentSessionIDFn = func() string {
	// Forward declaration: setupRuntime calls
	// tasks.SetCurrentSessionID(sid). We pull through that side-effect
	// channel rather than threading sessionID through every render
	// signature.
	return tasksCurrentSessionIDImpl()
}

func countEditDiff(input map[string]any) (added, removed int) {
	// Same dual-field-name read as renderEditDiff above.
	oldS, _ := input["old"].(string)
	newS, _ := input["new"].(string)
	if oldS == "" && newS == "" {
		oldS, _ = input["old_string"].(string)
		newS, _ = input["new_string"].(string)
	}
	edits := udiff.Strings(oldS, newS)
	uout, err := udiff.ToUnifiedDiff("", "", oldS, edits, 0)
	if err != nil {
		return
	}
	for _, h := range uout.Hunks {
		for _, ln := range h.Lines {
			switch ln.Kind {
			case udiff.Insert:
				added++
			case udiff.Delete:
				removed++
			}
		}
	}
	return
}
