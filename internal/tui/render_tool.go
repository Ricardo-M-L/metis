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
	"image/color"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	udiff "github.com/aymanbagabas/go-udiff"
	xansi "github.com/charmbracelet/x/ansi"

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
	return renderToolEventAtWidth(te, expanded, 0)
}

// renderToolEventAtWidth keeps the leader row inside the chat viewport before
// the terminal's last-resort frame clamp runs. In particular, long Bash
// commands and CJK paths are middle-truncated so both the command prefix and
// the final file/subcommand remain visible, matching Codex CLI's compact tool
// rows without creating a physical soft-wrap row.
func renderToolEventAtWidth(te ToolEvent, expanded bool, width int) string {
	var s strings.Builder
	baseName := strings.TrimPrefix(te.ToolName, "sub: ")
	partialRecovery := completedBeforeTimeout(te)
	neutralNoMatch := benignReadOnlyNoMatch(te)
	// ToolEvent is passed by value: normalize the user-visible copy once while
	// preserving the raw event for the model, transcript, and audit log.
	te.Output = normalizeToolOutput(te.Output)
	timeoutBody, timeoutLimit, timedOut := splitCommandTimeoutOutput(te.Output)
	// Permission denials get their own row + body treatment (see below);
	// computed once up front so leader, result row, and body all agree.
	denied := te.IsError && isDeniedToolResult(te.Output)
	// Safety/classifier blocks ([blocked] …) get the same compact row
	// treatment, labelled "Blocked" instead of "Denied".
	blocked := te.IsError && isBlockedToolResult(te.Output)

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
		if neutralNoMatch {
			leaderColor = styleDim
		} else if partialRecovery {
			leaderColor = styleWarn
		} else if te.IsError {
			leaderColor = styleErr
		} else {
			leaderColor = styleSuccess
		}
	}
	// Sub-agent tool calls (forwarded from a child Agent loop, carrying
	// a SubAgentParentID + a "sub: " name prefix) render INDENTED under
	// their parent agent row, so the transcript reads as a tree —
	// "agent(x)" then indented "glob", "grep" — instead of a flat
	// "agent(x)" followed by top-level "sub: glob". The extra indent
	// (+4) on both the leader and result rows is the visual nesting;
	// the "sub: " prefix is then redundant and stripped, since the
	// indentation already says "this came from the sub-agent above".
	// Mirrors claude-code's nested sub-agent display.
	isSub := te.SubAgentParentID != ""
	leadIndent, resultIndent := "  ", "    "
	displayName := te.ToolName
	if isSub {
		leadIndent, resultIndent = "      ", "        "
		displayName = strings.TrimPrefix(displayName, "sub: ")
	}
	displayTool := displayToolName(displayName)
	args := toolArgsPreview(te.ToolName, te.Input)
	if args != "" && width > 0 {
		trailingWidth := 2 // parentheses
		if te.Kind == "start" {
			trailingWidth += 2 // " …"
		}
		fixedWidth := xansi.StringWidth(leadIndent+glyphBullet+" "+displayTool) + trailingWidth
		args = truncateMiddleCells(args, width-fixedWidth)
	}
	s.WriteString(leaderColor.Render(leadIndent + glyphBullet + " "))
	s.WriteString(styleToolName.Render(displayTool))
	if args != "" {
		// Brackets are pure structural chrome — stay muted. The args
		// payload inside them is what the user came to read (path /
		// query / URL), so it renders at default fg (user screenshot
		// 38, 2026-05-18: "glob(**/.metis/**/*.toml) 蓝色框画出来的
		// 为啥还是灰色"). Same rule that lifted the result-summary
		// line to default fg in screenshot 36.
		s.WriteString(styleMuted.Render("("))
		// WebFetch: wrap the truncated URL display with OSC 8 so
		// terminals that support it (iTerm2, WezTerm, Alacritty,
		// GNOME, Windows Terminal) make the URL clickable. Other
		// terminals see the same text and just no-op the escape.
		if te.ToolName == "WebFetch" {
			if url, ok := te.Input["url"].(string); ok && url != "" {
				s.WriteString(osc8Link(args, url))
			} else {
				s.WriteString(args)
			}
		} else {
			s.WriteString(args)
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
	//
	// The leaf glyph + ✓/✗ stay structurally subdued (dim / accent), but
	// the summary text itself ("0.5s · Read foo.py (350 lines)") now
	// renders at default fg — user screenshot 36 / 2026-05-17 flagged
	// the dim grey as too low-contrast. This is the most-scanned line
	// per tool call and it's informational, not chrome.
	if denied {
		// Denied: keep ⎿ leaf glyph at leadIndent (flat, not nested).
		// The earlier resultIndent (leadIndent + 4-space) made it look
		// like a code-block continuation instead of a flat label.
		s.WriteString(styleDim.Render(leadIndent + glyphTreeLeaf + " Denied"))
	} else if blocked {
		// Same: ⎿ glyph at leadIndent.
		s.WriteString(styleDim.Render(leadIndent + glyphTreeLeaf + " Blocked"))
	} else {
		s.WriteString(styleDim.Render(resultIndent + glyphTreeLeaf + "  "))
		if neutralNoMatch {
			s.WriteString(styleDim.Render("○ "))
		} else if partialRecovery {
			s.WriteString(styleWarn.Render("⇻ "))
		} else if te.IsError {
			s.WriteString(styleErr.Render("✗ "))
		} else {
			s.WriteString(styleAccent.Render("✓ "))
		}
	}
	if denied || blocked {
		// Row already carries "Denied"/"Blocked"; nothing else to append.
	} else if neutralNoMatch {
		s.WriteString(fmt.Sprintf("%s · No matches", formatElapsed(te.Duration)))
	} else if partialRecovery {
		s.WriteString(summarizePartialToolResult(te))
	} else if timedOut {
		// The timeout marker already carries the meaningful wall-clock limit.
		// Showing the event Duration as well produced contradictory rows such as
		// "0ms · [command exceeded timeout 20s]" when a resumed/forwarded event
		// did not retain its start time. Render one semantic result instead.
		s.WriteString("timed out after " + timeoutLimit)
	} else {
		s.WriteString(summarizeNormalizedToolResult(te))
	}
	if denied || blocked {
		// No ctrl+O hint: the reason below is already the full content.
	} else if neutralNoMatch {
		// A read-only search with the conventional exit code 1 means
		// "nothing found". There is no diagnostic body to expand.
	} else if partialRecovery && !expanded {
		s.WriteString(styleMuted.Render(" (ctrl+O to inspect timeout)"))
	} else if !te.IsError && !expanded && te.Output != "" && collapseToolBodyByDefault(baseName) {
		s.WriteString(styleMuted.Render(" (ctrl+O to expand)"))
	}
	s.WriteString("\n")

	// Body: structured diff for Edit/Write, task-list for TodoWrite,
	// line-capped preview otherwise. On error, surface the error
	// output so the user can see WHY it failed.
	if te.IsError {
		if denied {
			// The row says "Denied" — do not repeat the "denied: …"
			// envelope in the body. Render just the reason as muted
			// prose (claude-code's RejectedToolUseMessage is dim
			// text, not an error-red wall).
			if reason := stripDenyEnvelope(te.Output); reason != "" {
				s.WriteString(renderDenyReasonBody(reason, expanded))
			}
		} else if blocked {
			// Same treatment for safety blocks: strip the
			// "[blocked] …" wrapper, show the human reason only.
			if reason := stripBlockedEnvelope(te.Output); reason != "" {
				s.WriteString(renderDenyReasonBody(reason, expanded))
			}
		} else if timedOut && !partialRecovery {
			// Bash appends a machine-oriented timeout marker after any captured
			// stdout/stderr. The result row above communicates that state once;
			// keep useful pre-timeout output without echoing the marker as a red
			// diagnostic line underneath it.
			if timeoutBody != "" {
				s.WriteString(renderNormalizedErrorBody(timeoutBody, expanded))
			}
		} else if te.Output != "" && !neutralNoMatch && (!partialRecovery || expanded) {
			s.WriteString(renderNormalizedErrorBody(te.Output, expanded))
		}
	} else {
		// Strip the forwarded-sub-agent "sub: " prefix when routing
		// to tool-specific renderers. Pre-2026-05-21 the switch
		// matched exact "Edit" / "Write" / "TodoWrite", so any
		// "sub: Edit" / "sub: Write" from a child agent fell through
		// to the bare-Output default and the user saw "wrote
		// /path/to/file" instead of the red/green diff or content
		// preview (image #49 repro). The display name above keeps
		// the "sub: " prefix so the user still sees it came from a
		// sub-agent; only the renderer dispatch uses the base name.
		switch baseName {
		case "Edit":
			s.WriteString(renderEditDiff(te.Input, expanded))
		case "Write":
			s.WriteString(renderWritePreview(te.Input, expanded))
		case "TodoWrite":
			s.WriteString(renderTodoSnapshot())
		case "SubAgentOutput":
			// SubAgentOutput's LLM-facing body starts with a machine KV
			// header ("agent_id=agt-XXX name=YYY status=running …")
			// followed by "---output---" / "---end---" markers. That
			// envelope is useful to the model but reads as noise in the
			// TUI — strip those lines and render only the agent's own
			// text content.
			filtered := stripSubAgentOutputEnvelope(te.Output)
			if filtered != "" {
				s.WriteString(renderNormalizedToolOutputPreview(te.ToolName, filtered, expanded))
			}
		default:
			if te.Output != "" && (expanded || !collapseToolBodyByDefault(baseName)) {
				s.WriteString(renderNormalizedToolOutputPreview(te.ToolName, te.Output, expanded))
			}
		}
	}
	s.WriteString("\n")
	return s.String()
}

// truncateMiddleCells is the cell-width counterpart of truncateMiddle. Rune
// caps are insufficient for Chinese paths because a rune may occupy two
// terminal cells. The head receives 40% of the budget and the tail 60% so a
// Bash verb remains recognizable while the target filename is preserved.
func truncateMiddleCells(value string, maxCells int) string {
	if maxCells <= 0 {
		return ""
	}
	if xansi.StringWidth(value) <= maxCells {
		return value
	}
	const separator = " … "
	separatorWidth := xansi.StringWidth(separator)
	if maxCells <= separatorWidth+2 {
		return xansi.Truncate(value, maxCells, "…")
	}
	headBudget := (maxCells - separatorWidth) * 2 / 5
	tailBudget := maxCells - separatorWidth - headBudget
	head := xansi.Truncate(value, headBudget, "")

	runes := []rune(value)
	tailStart := len(runes)
	tailWidth := 0
	for tailStart > 0 {
		candidate := string(runes[tailStart-1])
		candidateWidth := xansi.StringWidth(candidate)
		if tailWidth+candidateWidth > tailBudget {
			break
		}
		tailStart--
		tailWidth += candidateWidth
	}
	return head + separator + string(runes[tailStart:])
}

// collapseToolBodyByDefault matches Claude Code's compact transcript: high-
// volume exploration and delegated-agent results render as a one-line summary
// until Ctrl+O switches to transcript mode. Metis previously printed the first
// five lines of every such result followed by "… +N more lines", producing a
// wall of ellipses even though the per-turn recap already summarized the work.
// Errors remain expanded enough to diagnose, and Edit/Write/Bash keep their
// dedicated visible renderers.
func collapseToolBodyByDefault(toolName string) bool {
	switch toolName {
	case "Read", "LS", "Glob", "Grep", "WebSearch", "WebFetch", "Agent", "Fork",
		"SubAgentList", "SubAgentStop":
		return true
	// SubAgentOutput is intentionally NOT collapsed by default: the
	// point of polling a sub-agent output is to READ what it produced,
	// so hiding it behind ctrl+O defeats the purpose (user feedback
	// 2026-08-01). claude-code collapses it; we diverge because
	// SubAgent results are usually the final answer the user asked for.
	case "SubAgentOutput":
		return false
	default:
		return false
	}
}

var (
	terminalLineResetRE = regexp.MustCompile(`\x1b\[[0-9;?]*[GK]`)
	commandTimeoutRE    = regexp.MustCompile(`(?i)^\[command exceeded timeout ([^]\r\n]+)\]$`)
	// The completion marker is deliberately line-anchored and positive. It
	// must not accept prose such as "not installed 1 skill" or the vacuous
	// "Installed 0 skills".
	installedSkillsMarkerRE = regexp.MustCompile(`(?im)^[[:space:]│◇◆✓✔└├┌┐┘┬┴─•*-]*installed[[:space:]]+[1-9][0-9]*[[:space:]]+skills?\b`)
	skillsAddCommandRE      = regexp.MustCompile(`(?i)(?:^|[;&|][[:space:]]*)npx(?:[[:space:]]+(?:--yes|-y))*[[:space:]]+skills[[:space:]]+add(?:[[:space:]]|$)`)
	hyperframesUpdateRE     = regexp.MustCompile(`(?i)(?:^|[;&|][[:space:]]*)npx(?:[[:space:]]+(?:--yes|-y))*[[:space:]]+hyperframes(?:@[^[:space:]]+)?[[:space:]]+skills[[:space:]]+update(?:[[:space:]]|$)`)
)

// terminalSpinnerRunes covers the two frame families seen most often in
// package managers (ora's quarter circles and the common braille spinner).
// They are only treated specially when a line contains at least two frames,
// so ordinary prose containing one decorative glyph is left alone.
const terminalSpinnerRunes = "◒◐◓◑⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"

// normalizeToolOutput turns captured terminal animation into the text a user
// would have seen after the command settled. PTY programs repaint one logical
// row with CR and CSI erase/cursor-home sequences; storing those bytes and then
// rendering them as plain text produced hundreds of concatenated spinner
// frames in Metis. We preserve every non-animation status/error line, collapse
// a repaint row to its final frame, and strip foreign ANSI before applying the
// Metis palette.
func normalizeToolOutput(out string) string {
	if out == "" {
		return ""
	}
	// Treat cursor-home / erase-line as a repaint boundary before stripping
	// ANSI. Once stripped, adjacent frames would otherwise be inseparable.
	out = terminalLineResetRE.ReplaceAllString(out, "\r")
	out = xansi.Strip(out)
	out = strings.ReplaceAll(out, "\r\n", "\n")

	rawLines := strings.Split(out, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, raw := range rawLines {
		// CR means "return to column zero and overwrite this row". Keep the
		// last non-empty frame, which is the settled terminal state.
		frame := raw
		if strings.Contains(raw, "\r") {
			frame = ""
			for _, candidate := range strings.Split(raw, "\r") {
				if strings.TrimSpace(candidate) != "" {
					frame = candidate
				}
			}
		}
		frame = collapseInlineSpinnerFrames(frame)
		// ANSI cursor-up renderers can leave one spinner frame per logical
		// line after escape stripping. Keep only the last consecutive frame.
		if isSpinnerProgressLine(frame) && len(lines) > 0 && isSpinnerProgressLine(lines[len(lines)-1]) {
			lines[len(lines)-1] = frame
			continue
		}
		// Do not deduplicate ordinary adjacent lines. Repeated test output,
		// counters, and printf data are real evidence; only explicit terminal
		// repaint boundaries and recognizable spinner frames may collapse.
		lines = append(lines, frame)
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// splitCommandTimeoutOutput recognizes the exact terminal marker emitted by
// the Bash tool and separates it from any output produced before the process
// was killed. Matching only the final line prevents ordinary prose mentioning
// a timeout from being rewritten. The caller operates on a display-only copy;
// persisted/model-visible tool output remains byte-for-byte unchanged.
func splitCommandTimeoutOutput(out string) (body, limit string, ok bool) {
	trimmed := strings.TrimRight(out, " \t")
	lineStart := strings.LastIndexByte(trimmed, '\n') + 1
	marker := strings.TrimSpace(trimmed[lineStart:])
	match := commandTimeoutRE.FindStringSubmatch(marker)
	if len(match) != 2 || strings.TrimSpace(match[1]) == "" {
		return out, "", false
	}
	body = strings.TrimRight(trimmed[:lineStart], "\n")
	return body, strings.TrimSpace(match[1]), true
}

func isSpinnerRune(r rune) bool {
	return strings.ContainsRune(terminalSpinnerRunes, r)
}

func isSpinnerProgressLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(trimmed)
	if isSpinnerRune(r) {
		return true
	}
	// Captured output can be tail-capped in the middle of a frame, leaving a
	// short fragment before the next spinner glyph (for example `G◒ Cloning`).
	// Three or more frame glyphs still prove this is terminal animation.
	count := 0
	for _, candidate := range trimmed {
		if isSpinnerRune(candidate) {
			count++
		}
	}
	return count >= 3
}

func collapseInlineSpinnerFrames(line string) string {
	trimmed := strings.TrimSpace(line)
	firstRune, _ := utf8.DecodeRuneInString(trimmed)
	startsWithSpinner := isSpinnerRune(firstRune)
	count, lastByte := 0, -1
	for byteIdx, r := range line {
		if isSpinnerRune(r) {
			count++
			lastByte = byteIdx
		}
	}
	if count < 2 || lastByte < 0 || (!startsWithSpinner && count < 3) {
		return line
	}
	return line[lastByte:]
}

func outputContainsTimeout(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "command exceeded timeout") ||
		strings.Contains(low, "timed out after") ||
		strings.Contains(low, "timeout exceeded") ||
		strings.Contains(low, "deadline exceeded")
}

func knownSkillInstallerCommand(te ToolEvent) bool {
	if strings.TrimPrefix(te.ToolName, "sub: ") != "Bash" {
		return false
	}
	command, _ := te.Input["command"].(string)
	return skillsAddCommandRE.MatchString(command) || hyperframesUpdateRE.MatchString(command)
}

func positiveInstalledSkillsMarker(out string) (int, bool) {
	loc := installedSkillsMarkerRE.FindStringIndex(out)
	if loc == nil {
		return 0, false
	}
	return loc[1], true
}

func fatalInstallEvidenceAfter(out string, markerEnd int) bool {
	if markerEnd < 0 || markerEnd > len(out) {
		return true
	}
	tail := strings.ToLower(out[markerEnd:])
	for _, marker := range []string{
		"authentication failed", "permission denied", "validation failed",
		"installation failed", "failed to", "failure", "fatal", "error:",
		"forbidden", "unauthorized", "access denied", "denied by",
		"unable to", "could not", "not installed", "invalid", "canceled", "cancelled",
	} {
		if strings.Contains(tail, marker) {
			return true
		}
	}
	return false
}

// completedBeforeTimeout is deliberately conservative: timeout alone is an
// error, and vague output such as "done" is not enough. Only a strong install
// completion marker plus an outer timeout qualifies as partial/recovered. The
// original IsError bit and output remain untouched for the model, transcript,
// audit log, and expanded rendering.
func completedBeforeTimeout(te ToolEvent) bool {
	if !te.IsError || importantToolError(te) {
		return false
	}
	return completedBeforeTimeoutAfterImportanceCheck(te)
}

// completedBeforeTimeoutAfterImportanceCheck avoids rescanning a large PTY
// capture when the caller has already excluded permission/security failures.
func completedBeforeTimeoutAfterImportanceCheck(te ToolEvent) bool {
	if !te.IsError || !knownSkillInstallerCommand(te) {
		return false
	}
	// Completion/timeout markers are plain text even when the surrounding
	// progress UI uses ANSI. A positive installer count must precede the timeout,
	// and no fatal/validation evidence may follow it. This remains a reported
	// partial completion, never proof that the installed skill is usable.
	markerEnd, ok := positiveInstalledSkillsMarker(te.Output)
	if !ok || !outputContainsTimeout(te.Output[markerEnd:]) || fatalInstallEvidenceAfter(te.Output, markerEnd) {
		return false
	}
	return true
}

func summarizePartialToolResult(te ToolEvent) string {
	return fmt.Sprintf("%s · install reported complete before timeout; verify", formatElapsed(te.Duration))
}

func onlyExitStatusOne(out string) bool {
	if len(out) > 256 {
		return false
	}
	low := strings.ToLower(strings.TrimSpace(normalizeToolOutput(out)))
	low = strings.TrimSpace(strings.TrimPrefix(low, "error:"))
	switch low {
	case "[exit status 1]", "exit status 1", "[command exited with code 1]", "command exited with code 1":
		return true
	default:
		return false
	}
}

func shellProgramName(field string) string {
	field = strings.Trim(field, "'\"")
	if slash := strings.LastIndexByte(field, '/'); slash >= 0 {
		field = field[slash+1:]
	}
	return field
}

// readOnlySearchCommand is intentionally narrower than a shell parser. It
// recognizes the no-match convention only when every command/pipeline segment
// starts with a known read-only search or output-filter program and contains no
// mutation primitive. Unknown shell syntax stays an error.
func readOnlySearchCommand(command string) bool {
	low := strings.ToLower(strings.TrimSpace(command))
	if low == "" {
		return false
	}
	// This classifier is not a shell parser. Command/process substitution and
	// grouping can hide an arbitrary side effect behind an apparently harmless
	// grep/find segment, so unknown shell syntax fails closed.
	if strings.Contains(low, "$(") || strings.Contains(low, "`") ||
		strings.Contains(low, "<(") || strings.Contains(low, ">(") ||
		strings.Contains(low, "(") || strings.Contains(low, ")") {
		return false
	}
	for _, unsafe := range []string{
		" -delete", " -exec", " -execdir", " -fls", " -fprint", " -fprint0", " -fprintf",
		" -ok", " -okdir", " --delete", " --pre",
		" sed -i", " sed --in-place", " sort -o", " sort --output",
		" rm ", " mv ", " cp ", " tee ", " truncate ",
	} {
		if strings.Contains(" "+low+" ", unsafe) {
			return false
		}
	}
	// A redirect to /dev/null is diagnostic suppression, not a write. Any
	// remaining output redirect means the command has a side effect.
	withoutDevNull := strings.NewReplacer(
		"2>/dev/null", "", "2> /dev/null", "", ">/dev/null", "", "> /dev/null", "",
	).Replace(low)
	if strings.Contains(withoutDevNull, ">") {
		return false
	}

	segments := strings.FieldsFunc(low, func(r rune) bool {
		return r == ';' || r == '|' || r == '&' || r == '\n'
	})
	if len(segments) == 0 {
		return false
	}
	allowed := map[string]struct{}{
		"cut": {}, "egrep": {}, "fgrep": {}, "find": {}, "grep": {},
		"head": {}, "rg": {}, "tail": {}, "uniq": {}, "wc": {},
	}
	sawSearch := false
	for _, segment := range segments {
		fields := strings.Fields(strings.TrimSpace(segment))
		if len(fields) == 0 {
			continue
		}
		first := 0
		for first < len(fields) && strings.Contains(fields[first], "=") && !strings.HasPrefix(fields[first], "-") {
			first++
		}
		if first >= len(fields) {
			return false
		}
		program := shellProgramName(fields[first])
		if _, ok := allowed[program]; !ok {
			return false
		}
		switch program {
		case "find", "grep", "egrep", "fgrep", "rg":
			sawSearch = true
		}
	}
	return sawSearch
}

// benignReadOnlyNoMatch maps the conventional grep/rg exit 1 (and the same
// status from a stderr-suppressed find chain) to a neutral empty result. The
// IsError bit remains unchanged for audit/model semantics; only the TUI avoids
// presenting "no match" as a red execution failure.
func benignReadOnlyNoMatch(te ToolEvent) bool {
	if !te.IsError || strings.TrimPrefix(te.ToolName, "sub: ") != "Bash" || !onlyExitStatusOne(te.Output) {
		return false
	}
	command, _ := te.Input["command"].(string)
	return readOnlySearchCommand(command)
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
	return renderNormalizedErrorBody(normalizeToolOutput(out), expanded)
}

func renderNormalizedErrorBody(out string, expanded bool) string {
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

// bestErrorSummaryLine selects the most actionable diagnostic from terminal
// output instead of blindly taking its first non-empty row. Installers often
// start with box-drawing chrome or "Source: ..." and put the real cause at the
// end (for example, "Authentication failed ..."). Ties prefer the later line,
// matching conventional CLI error epilogues.
func bestErrorSummaryLine(out string) string {
	best, bestScore := "", -1
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		line = strings.TrimSpace(strings.TrimLeft(line, "│◇◆■□└├┌┐┘┬┴─•*-"))
		if line == "" || isSpinnerProgressLine(line) {
			continue
		}
		low := strings.ToLower(line)
		score := 10
		switch {
		case strings.Contains(low, "authentication failed"),
			strings.Contains(low, "permission denied"),
			strings.Contains(low, "denied by permission"),
			strings.Contains(low, "security rule"):
			score = 100
		case strings.Contains(low, "failed to clone"), strings.Contains(low, "installation failed"):
			score = 90
		case strings.Contains(low, "fatal"), strings.Contains(low, "error:"):
			score = 85
		case strings.Contains(low, "not found"), strings.Contains(low, "unable to"), strings.Contains(low, "could not"):
			score = 80
		case strings.Contains(low, "failed"), strings.Contains(low, "failure"):
			score = 70
		case strings.Contains(low, "timeout"), strings.Contains(low, "timed out"), strings.Contains(low, "deadline exceeded"):
			score = 60
		case strings.Contains(low, "exit status"), strings.Contains(low, "exited with code"):
			score = 50
		}
		if score >= bestScore {
			best, bestScore = line, score
		}
	}
	return best
}

// isDeniedToolResult detects permission-policy denials — the output
// dispatch.go emits for a PermissionDeny decision ("denied: <reason>"
// for the TUI, "denied by permission policy: <reason>" for the model).
// These get a compact "· denied" summary line instead of a 60-rune
// truncation of the diagnostic; the full reason renders in the error
// body row directly below, so nothing is lost — just not duplicated.
func isDeniedToolResult(out string) bool {
	low := strings.ToLower(strings.TrimSpace(normalizeToolOutput(out)))
	if low == "" {
		return false
	}
	return strings.HasPrefix(low, "denied") ||
		strings.Contains(low, "denied by permission policy")
}

// isBlockedToolResult detects safety/classifier refusals — the
// "[blocked] …" results the Bash tool emits for dangerous commands,
// denylist hits, and shellguard process-termination blocks. They get
// the same compact status-row treatment as permission denials: the
// row reads "Blocked", the reason renders as prose below. Before the
// 2026-08-15 rework these rendered as a truncated error summary plus
// a red wall that repeated the whole command ("[⚠️ blocked] command
// classified as dangerous: dangerous flag detected: (?i)-\s*rf\s
// \n\nCommand: …") — engine internals neither codex nor claude-code
// ever surface.
func isBlockedToolResult(out string) bool {
	low := strings.ToLower(strings.TrimSpace(normalizeToolOutput(out)))
	return strings.HasPrefix(low, "[blocked]") || strings.HasPrefix(low, "blocked:")
}

// stripBlockedEnvelope removes the "[blocked] " / "blocked: " wrapper
// so the body shows only the human reason.
func stripBlockedEnvelope(out string) string {
	o := strings.TrimSpace(normalizeToolOutput(out))
	o = strings.TrimSpace(strings.TrimPrefix(o, "[blocked]"))
	o = strings.TrimSpace(strings.TrimPrefix(o, "blocked:"))
	return o
}

// stripDenyEnvelope removes the wrapper dispatch.go adds around a deny
// reason ("denied: …", "denied by permission policy: …", "denied by
// user: …", optionally behind "Error: ") so the body shows only the
// reason itself. Only applied when the FIRST non-empty line carries
// the envelope — composite outputs (installer logs that happen to
// contain a deny line) stay untouched.
func stripDenyEnvelope(out string) string {
	o := strings.TrimSpace(normalizeToolOutput(out))
	o = strings.TrimSpace(strings.TrimPrefix(o, "Error:"))
	if !strings.HasPrefix(o, "denied") {
		return o
	}
	o = strings.TrimSpace(o[len("denied"):])
	if strings.HasPrefix(o, "by permission policy") {
		o = strings.TrimSpace(o[len("by permission policy"):])
	} else if strings.HasPrefix(o, "by user") {
		o = strings.TrimSpace(o[len("by user"):])
	}
	return strings.TrimLeft(o, ": ")
}

// renderDenyReasonBody renders the denial reason as muted prose —
// claude-code parity: its RejectedToolUseMessage / InterruptedByUser
// are dim text, not an error-red wall. The row above already says
// "Denied"; the reason is the informational part.
func renderDenyReasonBody(reason string, expanded bool) string {
	if reason == "" {
		return ""
	}
	lines := strings.Split(reason, "\n")
	const maxPreview = 5
	show := lines
	if !expanded && len(show) > maxPreview {
		show = show[:maxPreview]
	}
	var s strings.Builder
	for _, ln := range show {
		s.WriteString(styleDim.Render("       " + truncateMiddle(ln, 120)))
		s.WriteString("\n")
	}
	if !expanded && len(lines) > maxPreview {
		s.WriteString(styleMuted.Render(fmt.Sprintf("       … +%d more lines (ctrl+O to expand)", len(lines)-maxPreview)))
		s.WriteString("\n")
	}
	return s.String()
}

// summarizeToolResult crafts the per-tool one-line description that
// follows the ⎿ checkmark. Format is `<elapsed> · <tool-specific phrase>`.
func summarizeToolResult(te ToolEvent) string {
	// Summaries must describe the settled terminal output, not the first
	// captured animation frame. Keep normalization at the render boundary so
	// the model and persisted transcript still receive the original bytes.
	te.Output = normalizeToolOutput(te.Output)
	return summarizeNormalizedToolResult(te)
}

func summarizeNormalizedToolResult(te ToolEvent) string {
	// Use formatElapsed (tui_spinner.go) instead of bare
	// Duration.String() so a zero-or-sub-millisecond Duration
	// renders as "0ms" rather than "0s" — image #27 (2026-05-21):
	// user saw rows alternating "0s · Read foo.py" / "7ms · Read
	// bar.py" because the upstream Duration was sometimes literally
	// zero (when the tool finished faster than the millisecond
	// timer could capture, or the measurement was skipped on a
	// fast path) and time.Duration.String() formats 0 as "0s".
	// formatElapsed always picks the unit by magnitude, so 0
	// becomes "0ms" — same column-shape as the rest.
	dur := formatElapsed(te.Duration)
	// Permission denials get the compact status treatment (claude-code
	// parity): renderToolEvent's row reads icon-less "Denied" with the
	// full reason in the body below; this branch keeps the /session
	// summary view consistent. No elapsed time — a permission refusal
	// is not a timed operation, and "0ms · denied" only added noise
	// (user feedback 2026-08-15: "✗ 2ms · denied" 很丑).
	if te.IsError && isDeniedToolResult(te.Output) {
		return "denied"
	}
	// Safety/classifier blocks keep the same compact convention in the
	// /session summary view.
	if te.IsError && isBlockedToolResult(te.Output) {
		return "blocked"
	}
	// Strip "sub: " for the switch dispatch so per-tool summaries
	// (Read line count, Edit added/removed, Write line count, etc.)
	// fire for forwarded sub-agent events too. Pre-2026-05-21 a
	// "sub: Write" fell to the default branch and lost the
	// "Wrote /file (N lines)" detail. See sibling fix in
	// renderToolEvent above.
	switch strings.TrimPrefix(te.ToolName, "sub: ") {
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
		if te.IsError {
			if diagnostic := bestErrorSummaryLine(te.Output); diagnostic != "" {
				return fmt.Sprintf("%s · %s", dur, truncate(diagnostic, 60))
			}
			return fmt.Sprintf("%s · failed", dur)
		}
		first := firstNonEmptyLine(te.Output)
		if first == "" {
			return dur
		}
		return fmt.Sprintf("%s · %s", dur, truncate(first, 60))
	case "Glob":
		// Error-path: te.Output is the error message (e.g. the
		// "pattern field is required" hint when the model called Glob
		// with a `path` and no `pattern`), so lineCount(Output) would
		// report "Found N files" for the N lines of error text — a
		// contradictory "✗ Found 3 files". Surface the failure instead,
		// matching the Read branch above.
		if te.IsError {
			return fmt.Sprintf("%s · failed", dur)
		}
		if strings.HasPrefix(strings.TrimSpace(te.Output), "(no matches)") {
			return fmt.Sprintf("%s · No files matched", dur)
		}
		n := lineCount(te.Output)
		word := "files"
		if n == 1 {
			word = "file"
		}
		return fmt.Sprintf("%s · Found %d %s", dur, n, word)
	case "SubAgentOutput":
		// SubAgentOutput's LLM-facing output begins with a machine KV
		// line "agent_id=agt-XXX name=YYY status=running …" — useful
		// for the model (which needs the ID to refer back) but noise
		// in the TUI. claude-code's TaskOutput leader shows a semantic
		// label ("Task is still running…"), codex shows the agent
		// nickname + friendly state. We do the same: the leader
		// surfaces the STATUS + how much output the child has
		// produced, NOT the internal IDs. The full KV line still
		// reaches the LLM; only the user-visible row is filtered.
		if te.IsError {
			return fmt.Sprintf("%s · failed", dur)
		}
		out := strings.TrimSpace(te.Output)
		if out == "" {
			return fmt.Sprintf("%s · no output yet", dur)
		}
		status := ""
		// Parse "status=X" from the FIRST whitespace-separated KV line.
		// Two shapes:
		//   - multi-line output: head = up to first "\n"
		//   - single-line output (just spawned, no body yet): head = whole out
		head := out
		if nl := strings.Index(out, "\n"); nl > 0 {
			head = out[:nl]
		}
		for _, kv := range strings.Fields(head) {
			if strings.HasPrefix(kv, "status=") {
				status = strings.TrimPrefix(kv, "status=")
				break
			}
		}
		body := ""
		if idx := strings.Index(out, "---output---"); idx >= 0 {
			body = out[idx+len("---output---"):]
		}
		n := lineCount(strings.TrimSpace(body))
		word := "lines"
		if n == 1 {
			word = "line"
		}
		switch status {
		case "running":
			if n > 0 {
				return fmt.Sprintf("%s · still running · %d %s", dur, n, word)
			}
			return fmt.Sprintf("%s · still running", dur)
		case "completed":
			return fmt.Sprintf("%s · completed · %d %s", dur, n, word)
		case "killed":
			return fmt.Sprintf("%s · killed", dur)
		case "failed":
			return fmt.Sprintf("%s · failed", dur)
		}
		return fmt.Sprintf("%s · %d %s", dur, n, word)
	case "SubAgentList":
		// SubAgentList returns a roster dump of "agent_id=... name=..."
		// KV lines — one per agent. Surface just the COUNT in the
		// leader; the user can ctrl+O for the roster detail.
		if te.IsError {
			return fmt.Sprintf("%s · failed", dur)
		}
		out := strings.TrimSpace(te.Output)
		if out == "" || strings.HasPrefix(out, "(no sub-agents") {
			return fmt.Sprintf("%s · no sub-agents", dur)
		}
		n := lineCount(out)
		word := "agents"
		if n == 1 {
			word = "agent"
		}
		return fmt.Sprintf("%s · %d %s", dur, n, word)
	case "SubAgentStop":
		if te.IsError {
			return fmt.Sprintf("%s · failed", dur)
		}
		return fmt.Sprintf("%s · stopped", dur)
	case "WebSearch":
		// Output begins with `WebSearch "<query>" — N results:` (see
		// websearch.go formatter). Parse N + the trailing "[via X]"
		// footer so the leader reads "12ms · 5 results · via tavily"
		// instead of dumping the first 60 chars of raw output. Mirrors
		// DeepSeek-TUI's `Found N result(s)` summary line.
		if te.IsError {
			if diagnostic := bestErrorSummaryLine(te.Output); diagnostic != "" {
				return fmt.Sprintf("%s · %s", dur, truncate(diagnostic, 60))
			}
			return fmt.Sprintf("%s · failed", dur)
		}
		out := strings.TrimSpace(te.Output)
		if out == "" {
			return fmt.Sprintf("%s · 0 results", dur)
		}
		backend := ""
		if idx := strings.LastIndex(out, "[via "); idx >= 0 {
			tail := out[idx+len("[via "):]
			if end := strings.Index(tail, "]"); end > 0 {
				backend = tail[:end]
			}
		}
		n := -1
		// Parse "— N results" from the FIRST line. Two shapes:
		//   - multi-line output: head = up to first "\n"
		//   - single-line output (no "\n" — partial stream or tiny
		//     response): head = whole out
		// Pre-2026-07-27 the `nl > 0` guard skipped parse entirely
		// when the output was a single line, dropping the result count.
		head := out
		if nl := strings.Index(out, "\n"); nl > 0 {
			head = out[:nl]
		}
		// "— " is U+2014 em dash + space = 3+1 = 4 bytes, NOT 2.
		// SscanF-skipping via head[i+2:] lands INSIDE the em dash and
		// the %d never matches. Use len() so the byte math is right.
		if i := strings.Index(head, "— "); i >= 0 {
			var cnt int
			if _, err := fmt.Sscanf(head[i+len("— "):], "%d results", &cnt); err == nil {
				n = cnt
			}
		}
		word := "results"
		if n == 1 {
			word = "result"
		}
		switch {
		case n >= 0 && backend != "":
			return fmt.Sprintf("%s · %d %s · via %s", dur, n, word, backend)
		case n >= 0:
			return fmt.Sprintf("%s · %d %s", dur, n, word)
		case backend != "":
			return fmt.Sprintf("%s · via %s", dur, backend)
		}
		return fmt.Sprintf("%s · %s", dur, truncate(firstNonEmptyLine(out), 60))
	case "WebFetch":
		if te.IsError {
			if diagnostic := bestErrorSummaryLine(te.Output); diagnostic != "" {
				return fmt.Sprintf("%s · %s", dur, truncate(diagnostic, 60))
			}
			return fmt.Sprintf("%s · failed", dur)
		}
		out := strings.TrimSpace(te.Output)
		if out == "" {
			return fmt.Sprintf("%s · empty", dur)
		}
		// Body size is the most useful "what did I just download" signal.
		n := len(te.Output)
		switch {
		case n >= 1024*1024:
			return fmt.Sprintf("%s · %.1f MB", dur, float64(n)/(1024*1024))
		case n >= 1024:
			return fmt.Sprintf("%s · %.1f KB", dur, float64(n)/1024)
		default:
			return fmt.Sprintf("%s · %d B", dur, n)
		}
	case "Grep":
		// Same error-path guard as Glob/Read — don't count error-message
		// lines as if they were matches.
		if te.IsError {
			return fmt.Sprintf("%s · failed", dur)
		}
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
		if te.IsError {
			if diagnostic := bestErrorSummaryLine(out); diagnostic != "" {
				return fmt.Sprintf("%s · %s", dur, truncate(diagnostic, 60))
			}
			return fmt.Sprintf("%s · failed", dur)
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

	// Full-row background fill: lipgloss .Width(N) pads the rendered
	// output with spaces so the background colour extends to column N
	// even when the diff text is shorter. Matches claude-code / Cursor
	// diff rendering (image #56 / #57 user feedback 2026-05-23). The
	// per-row width is the chat surface width minus the gutter ("       %4d "
	// = 7 + 4 + 1 = 12 chars) and the "+ "/"- " marker (2 chars). If we
	// haven't received a WindowSizeMsg yet, fall back to 100 — a sensible
	// "wide enough to look like a diff row" default.
	rowWidth := lastKnownChatWidth()
	if rowWidth <= 0 {
		rowWidth = 100
	}
	innerWidth := rowWidth - 12 - 2
	if innerWidth < 30 {
		innerWidth = 30
	}
	addBgOnly := lipgloss.NewStyle().Background(themes.Current().DiffAddBg).Width(innerWidth)
	delBgOnly := lipgloss.NewStyle().Background(themes.Current().DiffDelBg).Width(innerWidth)
	addBgInline := lipgloss.NewStyle().Background(themes.Current().DiffAddBg)
	delBgInline := lipgloss.NewStyle().Background(themes.Current().DiffDelBg)
	addStrong := lipgloss.NewStyle().Background(themes.Current().DiffAddBg).Foreground(themes.Current().DiffAddFg).Bold(true)
	delStrong := lipgloss.NewStyle().Background(themes.Current().DiffDelBg).Foreground(themes.Current().DiffDelFg).Bold(true)
	gutterStyle := lipgloss.NewStyle().Foreground(textMuted)
	filename := pickLanguageFromInput(input)

	// 2026-05-23: chroma highlightLine emits `\x1b[0m` (full reset)
	// after every syntax token. When the rendered diff line has a
	// background applied at the OUTER level, each chroma reset wipes
	// the bg on the cells that follow — leaving visible "uncovered"
	// gaps inside the row (user screenshot 60 feedback "中间还有
	// 一部分肢体没有被覆盖到"). reinjectBg post-processes the body
	// to re-establish the bg ANSI code after every `\x1b[0m` so the
	// outer Width-padded wrapper produces a contiguous coloured row.
	addBgAnsi := bgAnsiPrefix(themes.Current().DiffAddBg)
	delBgAnsi := bgAnsiPrefix(themes.Current().DiffDelBg)

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
				// Build the row body (marker + content + inline
				// strong highlights) into a single string, then wrap
				// the entire row in delBgOnly (which has .Width set)
				// so the background fills to chat width — matches
				// claude-code / Cursor diff rendering.
				var rowBody strings.Builder
				rowBody.WriteString(delBgInline.Render("- "))
				if delMask != nil {
					rowBody.WriteString(applyMask(content, delMask,
						func(s string) string { return delBgInline.Render(s) },
						func(s string) string { return delStrong.Render(s) }))
				} else {
					rowBody.WriteString(delBgInline.Render(highlightLine(content, filename)))
				}
				s.WriteString(delBgOnly.Render(reinjectBg(rowBody.String(), delBgAnsi)))
				oldLine++
				i++
				if partner != nil {
					s.WriteString("\n")
					rendered++
					if rendered >= limit {
						break
					}
					s.WriteString(gutterStyle.Render(fmt.Sprintf("       %4d ", newLine)))
					var addBody strings.Builder
					addBody.WriteString(addBgInline.Render("+ "))
					addBody.WriteString(applyMask(*partner, insMask,
						func(s string) string { return addBgInline.Render(s) },
						func(s string) string { return addStrong.Render(s) }))
					s.WriteString(addBgOnly.Render(reinjectBg(addBody.String(), addBgAnsi)))
					newLine++
					i++
				}
			case udiff.Insert:
				s.WriteString(gutterStyle.Render(fmt.Sprintf("       %4d ", newLine)))
				var addBody strings.Builder
				addBody.WriteString(addBgInline.Render("+ "))
				addBody.WriteString(addBgInline.Render(highlightLine(content, filename)))
				s.WriteString(addBgOnly.Render(reinjectBg(addBody.String(), addBgAnsi)))
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
	// Width-padded variant — full-row green background like Cursor /
	// claude-code. See renderEditDiff for the rowWidth rationale.
	rowWidth := lastKnownChatWidth()
	if rowWidth <= 0 {
		rowWidth = 100
	}
	innerWidth := rowWidth - 12 - 2
	if innerWidth < 30 {
		innerWidth = 30
	}
	addBgOnly := lipgloss.NewStyle().Background(themes.Current().DiffAddBg).Width(innerWidth)
	addBgInline := lipgloss.NewStyle().Background(themes.Current().DiffAddBg)
	gutterStyle := lipgloss.NewStyle().Foreground(textMuted)
	filename := pickLanguageFromInput(input)

	lines := strings.Split(strings.TrimRight(c, "\n"), "\n")
	const maxShow = 10
	show := lines
	if !expanded && len(show) > maxShow {
		show = show[:maxShow]
	}
	// Same bg-reinject treatment as renderEditDiff — chroma's per-token
	// `\x1b[0m` would otherwise leave uncovered cells inside the green row.
	addBgAnsi := bgAnsiPrefix(themes.Current().DiffAddBg)
	var s strings.Builder
	for i, ln := range show {
		s.WriteString(gutterStyle.Render(fmt.Sprintf("       %4d ", i+1)))
		var rowBody strings.Builder
		rowBody.WriteString(addBgInline.Render("+ "))
		rowBody.WriteString(addBgInline.Render(highlightLine(truncateRunes(ln, 100), filename)))
		s.WriteString(addBgOnly.Render(reinjectBg(rowBody.String(), addBgAnsi)))
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
	return renderNormalizedToolOutputPreview(toolName, normalizeToolOutput(out), expanded)
}

func renderNormalizedToolOutputPreview(toolName, out string, expanded bool) string {
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

// stripSubAgentOutputEnvelope removes the machine-readable wrapper from
// SubAgentOutput's result body so only the agent's own text renders in
// the TUI. Dropped lines:
//   - the leading "agent_id=agt-XXX name=YYY status=ZZZ …" KV header
//   - the "---output---" section marker
//   - the trailing "---end (elapsed=…)--- [hint]" footer
//
// The LLM still receives the full envelope (this filter only runs at
// the render boundary); the user sees just the agent's transcript text.
func stripSubAgentOutputEnvelope(out string) string {
	var kept []string
	for i, ln := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(ln)
		// First line is the KV header when it begins with agent_id=.
		if i == 0 && strings.HasPrefix(trimmed, "agent_id=") {
			continue
		}
		if trimmed == "---output---" {
			continue
		}
		if strings.HasPrefix(trimmed, "---end ") && strings.HasSuffix(trimmed, "---") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
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
		// Glob has no inline content, so the whole line IS the answer
		// — render at default fg, not dim (user screenshot 36 /
		// 2026-05-17 flagged glob result paths as too grey to read).
		if toolName == "Glob" {
			return ln
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
			styled = strike.Render(taskItemLabel(it))
		case "in_progress":
			glyph = glyphTaskInProgress
			glyphStyle = inProgressGlyph
			styled = current.Render(taskItemLabel(it))
		default: // pending or unknown
			glyph = glyphTaskPending
			glyphStyle = pendingGlyph
			styled = pending.Render(taskItemLabel(it))
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

// chatWidthMu + lastChatWidth — published from tui_update's
// WindowSizeMsg handler so renderEditDiff / renderWritePreview can
// pad their diff rows to full width without a parameter chain.
//
// Mutex used over sync/atomic only because the read+default-fallback
// happens at most once per row render — contention is irrelevant.
var (
	chatWidthMu   sync.RWMutex
	lastChatWidth int
)

func setLastKnownChatWidth(w int) {
	chatWidthMu.Lock()
	lastChatWidth = w
	chatWidthMu.Unlock()
}

func lastKnownChatWidth() int {
	chatWidthMu.RLock()
	defer chatWidthMu.RUnlock()
	return lastChatWidth
}

// bgAnsiPrefix extracts the "background ANSI start" sequence lipgloss
// would emit for the given color. e.g. for #1a2a1a in 256-color terms,
// it returns "\x1b[48;5;234m" (or similar). Empty if lipgloss couldn't
// produce a styled output (e.g. NoColor profile).
//
// Trick: render a single space with bg-only, then strip the visible
// space + trailing reset to get only the start prefix. lipgloss's
// renderer is stable enough that this round-trip works reliably.
func bgAnsiPrefix(c color.Color) string {
	out := lipgloss.NewStyle().Background(c).Render(" ")
	// Typical shape: "\x1b[<bg>m \x1b[0m". Find the FIRST space —
	// everything before it is the bg prefix.
	idx := strings.IndexByte(out, ' ')
	if idx <= 0 {
		return ""
	}
	return out[:idx]
}

// reinjectBg makes the given bg-ANSI prefix "sticky" across internal
// resets. chroma's terminal256 formatter emits `\x1b[0m` after every
// syntax token, which would otherwise wipe the outer background and
// leave cells with no fill. By post-processing the chroma output to
// insert the bg prefix immediately after each reset, the background
// re-establishes for all following cells. The outer lipgloss wrapper
// still adds its own start prefix + final reset, so this is
// idempotent / safe to chain.
func reinjectBg(s, bgAnsi string) string {
	if bgAnsi == "" || s == "" {
		return s
	}
	if !strings.Contains(s, "\x1b[0m") {
		return s
	}
	return strings.ReplaceAll(s, "\x1b[0m", "\x1b[0m"+bgAnsi)
}
