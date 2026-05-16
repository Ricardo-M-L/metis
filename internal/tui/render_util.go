package tui

// render_util.go — small render-time helpers shared across the
// per-feature render files. Keep these pure (no Model state) so they
// can be called from anywhere without ordering surprises.

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

// escapeLeakPatterns matches the partial escape sequences we've seen
// leak through bubbletea's parser. Each pattern strips the *body* of a
// known terminal escape; the leading ESC bytes are consumed by
// bubbletea before they reach us, but the trailing data sometimes
// makes it into the textarea char-by-char.
//
// Patterns are deliberately specific to escape body shapes — using a
// generic "(11|10|4|110);" was too eager and matched the "10;" inside
// SGR mouse events like "<35;10;5M". Each pattern below is anchored to
// a body shape no plain user prompt would ever contain (rgb hex
// quartets, M/m terminators, cursor ?h/?l mode toggles).
//
// Order matters: longer / more-specific patterns first so a partial
// match doesn't strand stragglers.
var escapeLeakPatterns = []*regexp.Regexp{
	// OSC <num>;<num>;rgb:HEX  — palette-index color reply
	// (`]4;0;rgb:RRRR/GGGG/BBBB\`). Two-number form first so the
	// single-number pattern below doesn't shadow it.
	regexp.MustCompile(`\]?\d+;\d+;rgb:[0-9a-fA-F/]+(?:\x1b\\|\\)?`),
	// OSC <num>;rgb:HEX — bg / fg / cursor color reply. The user has
	// reported leaks in three forms across iTerm2 versions: full
	// `]11;rgb:...`, `11;rgb:...` (lost `]`), and even `1;rgb:...`
	// (the parser ate `]1` together). So we accept ANY leading digit
	// run before `;rgb:` rather than enumerating 10/11 specifically.
	regexp.MustCompile(`\]?\d+;rgb:[0-9a-fA-F/]+(?:\x1b\\|\\)?`),
	// OSC color-reset: 110/111/112. The numeric body is preceded by
	// either `]` (the OSC introducer made it through) or — when the
	// parser ate the introducer — by `\x1b` (the ESC byte itself).
	// Anchoring to one of these prefixes prevents the pattern from
	// devouring plain user input like "111" / "1110" / phone-number-
	// style digit runs (image+video user report 2026-05-07: typing
	// "1" five times oscillated between 1 and 11 because every third
	// keystroke produced "111", which this pattern silently scrubbed
	// when the prefix was optional). We also require a real terminator
	// (`\x1b\\` proper, or the lone `\` fallback we've seen leak)
	// so a bare "110" — completely valid digit run in user input —
	// is left alone.
	regexp.MustCompile(`(?:\]|\x1b)(?:110|111|112)(?:\x1b\\|\\)`),
	// SGR mouse event body: <button;col;row[Mm]
	regexp.MustCompile(`<\d+;\d+;\d+[Mm]`),
	// DEC private mode set/reset: [?2004h, [?25l, [?1006h ...
	regexp.MustCompile(`\[?\?[0-9;]+[hl]`),
	// Cursor-position report: [24;80R
	regexp.MustCompile(`\[?\d+;\d+R`),
	// Run of 3+ box-drawing chars from the unicode `Box Drawing`
	// + `Block Elements` blocks. These almost never appear in real
	// user input (drawing tables in chat is exotic), and they're
	// the visible aftermath of X10-format mouse events whose 3-byte
	// triplets get reinterpreted as Latin-1 → UTF-8 box chars when
	// bubbletea fails to consume them. Image #50 from the user was
	// 12+ consecutive `□` (control-picture char `␀` family /
	// box-drawing fallback).
	regexp.MustCompile(`[\x{2400}-\x{259F}\x{2580}-\x{259F}]{3,}`),
}

// scrubEscapeLeaks removes any partial-escape garbage from a string
// without disturbing genuine user input. Returns the cleaned string;
// caller compares to the original to decide whether to update the
// textarea.
func scrubEscapeLeaks(s string) string {
	for _, re := range escapeLeakPatterns {
		s = re.ReplaceAllString(s, "")
	}
	return s
}

// pastedImageTag matches the `[Image #N]` placeholder Ctrl-V inserts
// when an image is on the clipboard. claude-code's pattern: friendly
// tag in the input, real path resolved at submit so the agent can read
// it. The number is 1-based and indexes back into Model.imagePaste.
var pastedImageTag = regexp.MustCompile(`\[Image #(\d+)\]`)

// expandPastedImages walks `text`, replacing every `[Image #N]` tag
// with `[image: <path>]` resolved from the per-Model index. Tags with
// no entry in the index (stale `#N` typed by the user, or carried
// across a /clear) are left as-is so the user sees what's missing.
func expandPastedImages(text string, idx map[int]string) string {
	return pastedImageTag.ReplaceAllStringFunc(text, func(match string) string {
		sub := pastedImageTag.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		var n int
		_, err := fmt.Sscanf(sub[1], "%d", &n)
		if err != nil {
			return match
		}
		if path, ok := idx[n]; ok {
			return "[image: " + path + "]"
		}
		return match
	})
}

// toolArgsPreview produces the parenthesized argument summary that
// appears next to a tool name in the transcript: `Read(foo.go)`,
// `Bash(go test ./...)`, etc. Per-tool dispatch keeps the preview
// format meaningful — for Bash it's the command, for Read it's the
// basename, for Grep it's the pattern.
func toolArgsPreview(name string, input map[string]any) string {
	if input == nil {
		return ""
	}
	var preview string
	switch name {
	case "Read", "Write", "Edit":
		if v, ok := input["path"].(string); ok {
			preview = basename(v)
		}
	case "Glob":
		// Glob's args are (pattern, root, limit, max_depth) — not path.
		// Without this case the leader row showed `glob …` with no
		// pattern, hiding what the model was searching for. Format
		// "<pattern>" or "<root>:<pattern>" so the user sees both.
		if pat, ok := input["pattern"].(string); ok && pat != "" {
			preview = pat
			if root, _ := input["root"].(string); root != "" && root != "." {
				preview = root + ":" + pat
			}
		}
	case "Bash":
		if v, ok := input["command"].(string); ok {
			// Rune-based slice — `v[:42]` sliced through Chinese
			// command arguments (e.g. paths under
			// "/公司学习文件/...") at byte 42, which often lands
			// mid-codepoint and emits invalid UTF-8 followed by a
			// stray "…". The terminal renders that as a grey
			// corruption box. See truncate() doc + 2026-05-16
			// image #14 repro.
			rs := []rune(v)
			if len(rs) > 45 {
				preview = string(rs[:42]) + "…"
			} else {
				preview = v
			}
		}
	case "Grep":
		if v, ok := input["pattern"].(string); ok {
			preview = v
			if root, _ := input["root"].(string); root != "" && root != "." {
				preview = root + ":" + v
			}
		}
	case "WebFetch":
		if v, ok := input["url"].(string); ok {
			preview = v
		}
	default:
		for _, key := range []string{"path", "command", "query", "name", "url"} {
			if v, ok := input[key].(string); ok {
				preview = v
				break
			}
		}
	}
	return truncate(preview, 45)
}

func basename(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// truncate clips s to at most max RUNES (not bytes) so a multi-byte
// UTF-8 character (Chinese, emoji) isn't sliced mid-codepoint. The
// old byte-based form produced invalid UTF-8 fragments that the
// terminal rendered as garbled boxes plus broken ANSI follow-on —
// the 2026-05-16 user repro (image #14, `bash(ls -la .../公司学习...)`
// showed grey corruption right at the multi-byte cut point).
//
// Quick-path: when the byte length is already ≤ max the runes count
// can't exceed it either, so we skip the rune walk entirely. The
// rare "ASCII slightly longer than max" case still allocates a rune
// slice, but that's fine — these strings are short (preview lines,
// tool-args headers).
// formatContextPct renders the right-side status-bar percentage with a
// 99%+ ceiling. Reasons we clamp instead of showing the raw value:
//   - `used` is the larger of API-reported tokens and
//     EstimateContextTokens() (chars/4). The latter over-counts CJK
//     because a 3-byte UTF-8 char doesn't equal one token. The
//     2026-05-16 user repro showed 207k/200k = 107% on a session
//     that wasn't actually past the API cap.
//   - Anything past 99% is the same actionable signal ("compact soon
//     or risk a 4xx"); the specific number above that is noise.
//   - >100% looks like a bug to users — they reasonably expect the
//     API to reject any request over the cap, so a 107% display
//     erodes trust in every other metric we show.
func formatContextPct(used, cap int) string {
	if cap <= 0 {
		return ""
	}
	pct := used * 100 / cap
	if pct > 99 {
		return "99%+"
	}
	return fmt.Sprintf("%d%%", pct)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max-1]) + "…"
}

// truncateRunes is like truncate but counts runes so multi-byte
// characters (Chinese, emoji) don't get sliced mid-codepoint.
func truncateRunes(s string, max int) string {
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max-1]) + "…"
}

// truncateCells truncates s so its rendered terminal-cell width is
// at most maxCells, accounting for east-asian doublewidth characters
// and emoji. The returned string ends with "…" when truncation
// happened (the ellipsis itself takes 1 cell, so the visible body is
// at most maxCells-1 cells from the original input). This is what
// you want for "fit one row exactly"; truncateRunes counts code
// points and overshoots by 2× on CJK content.
func truncateCells(s string, maxCells int) string {
	if maxCells <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= maxCells {
		return s
	}
	out := make([]rune, 0, len(s))
	used := 0
	budget := maxCells - 1 // reserve 1 cell for the ellipsis
	for _, r := range s {
		w := runewidth.RuneWidth(r)
		if used+w > budget {
			break
		}
		out = append(out, r)
		used += w
	}
	return string(out) + "…"
}

// fuzzyMatch is the legacy palette filter — kept for backward compat
// with any callers that haven't migrated to the strict matchCommands
// path. Not used by the slash palette anymore (matchCommands does
// exact > prefix > contains, no description match).
func fuzzyMatch(str, pattern string) bool {
	if pattern == "" {
		return true
	}
	str = strings.ToLower(str)
	pattern = strings.ToLower(pattern)
	if strings.Contains(str, pattern) {
		return true
	}
	si := 0
	for _, c := range pattern {
		found := false
		for si < len(str) {
			if rune(str[si]) == c {
				si++
				found = true
				break
			}
			si++
		}
		if !found {
			return false
		}
	}
	return true
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

func firstNonEmptyLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// buildTurnRecap synthesizes a deterministic recap line from this
// turn's tool events. Returns empty when nothing notable happened
// (≤1 tool call) — short turns are self-evident and a recap would
// just be noise.
//
// claude-code's recap is LLM-generated narrative; we trade that prose
// for cheap structural fact ("edited foo.go · 2 reads · 1 bash").
// Free, deterministic, fast.
func buildTurnRecap(toolEvents []ToolEvent) string {
	if len(toolEvents) < 2 {
		return ""
	}
	counts := map[string]int{}
	files := map[string]bool{}
	bashCmds := []string{}

	for _, te := range toolEvents {
		if te.Kind != "result" || te.IsError {
			continue
		}
		switch te.ToolName {
		case "Edit", "Write":
			counts[te.ToolName]++
			if path := stringField(te.Input, "path", "file_path"); path != "" {
				files[basename(path)] = true
			}
		case "Read":
			counts["Read"]++
		case "Bash":
			counts["Bash"]++
			if c, ok := te.Input["command"].(string); ok && len(bashCmds) < 1 {
				bashCmds = append(bashCmds, truncate(c, 30))
			}
		case "Grep", "Glob":
			counts[te.ToolName]++
		}
	}

	var parts []string
	editN := counts["Edit"] + counts["Write"]
	if editN > 0 {
		var fileList []string
		for f := range files {
			fileList = append(fileList, f)
		}
		if len(fileList) > 0 && len(fileList) <= 3 {
			parts = append(parts, "edited "+strings.Join(fileList, ", "))
		} else if len(fileList) > 3 {
			parts = append(parts, fmt.Sprintf("edited %d files", len(fileList)))
		} else {
			parts = append(parts, fmt.Sprintf("%d edits", editN))
		}
	}
	if counts["Read"] > 0 {
		parts = append(parts, fmt.Sprintf("%d reads", counts["Read"]))
	}
	if counts["Bash"] > 0 {
		if len(bashCmds) > 0 && counts["Bash"] == 1 {
			parts = append(parts, "ran `"+bashCmds[0]+"`")
		} else {
			parts = append(parts, fmt.Sprintf("%d bash", counts["Bash"]))
		}
	}
	searchN := counts["Grep"] + counts["Glob"]
	if searchN > 0 {
		parts = append(parts, fmt.Sprintf("%d searches", searchN))
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}

// displayToolName lowercases first-party tool names for the transcript
// (Read → read, WebFetch → webfetch, TodoWrite → todowrite). MCP and
// plugin tools keep their original casing because they already use
// snake_case underscores (`mcp__stub__echo`, `plugin__name__action`)
// and lowercasing would just collapse the visual structure.
//
// Critically this is render-only — the registry, schemas, and wire-
// format names the LLM consumes stay PascalCase. Some Anthropic-
// trained models pattern-match on `Read` / `Bash` / `Edit` exactly,
// so renaming the registration would break tool-call accuracy.
func displayToolName(name string) string {
	if strings.Contains(name, "__") {
		return name
	}
	return strings.ToLower(name)
}
