package tui

import (
	"strings"

	xansi "github.com/charmbracelet/x/ansi"
)

// classifyREPLOutput maps a REPL command's plain-string output to the
// best-fit Message.Role (info / command-result / success / warning / error). Phase B
// added per-glyph styling for these roles; without classification, the
// REPL command dispatch hard-coded everything as "info" so success
// confirmations like "rename: session title set to X" rendered with
// the muted · prefix instead of the green ✓.
//
// Heuristic, prefix-driven:
//   - "error:" / "...: error " / "failed" → error
//   - "warning:" / "...: no " / "(warning..." → warning
//   - "set", "saved", "added", "removed", "synced", "title set to",
//     "renamed", "tagged", "switched", "applied", "exported",
//     "branched", "(yes)", "(allowed)" → success
//   - everything else → info
//
// Pattern-matching rather than enum-typed handlers because metis has
// 50+ cmdXxx functions and each one's just a one-liner; centralizing
// the role decision here is far less churn than retrofitting (string,
// Role) returns across the registry.
func classifyREPLOutput(out string) string {
	plain := strings.TrimSpace(xansi.Strip(out))
	// renderInfoBox already carries its own visual hierarchy and may contain
	// neutral words such as "unknown" in a single field. Classifying the whole
	// ANSI box by body text promotes the entire panel to warning/error and, more
	// importantly, makes the transcript renderer prepend a status glyph to a
	// bordered block. Keep structured informational panels in the info lane;
	// individual handlers should return a plain error/warning when the command
	// itself failed.
	if isRenderedInfoBox(out) {
		return "info"
	}
	low := strings.ToLower(plain)

	// error markers (loudest tier — check first).
	if strings.HasPrefix(low, "error:") ||
		strings.HasPrefix(low, "error ") ||
		strings.Contains(low, "failed") ||
		strings.HasPrefix(low, "(error") {
		return "error"
	}

	// warning markers — soft refusals / "no X available" hints.
	if strings.HasPrefix(low, "warning:") ||
		strings.HasPrefix(low, "(warning") ||
		strings.Contains(low, "no active session") ||
		strings.Contains(low, "no session store") ||
		strings.Contains(low, "not available") ||
		strings.Contains(low, "not implemented") ||
		strings.Contains(low, "unknown") ||
		strings.Contains(low, "not wired") ||
		strings.Contains(low, "use ") && strings.Contains(low, "to set") {
		return "warning"
	}

	// Claude Code renders a slash-command result in the tool-result lane
	// (`⎿  …`), not as a celebratory green check. Keep export's completion
	// line in that lane so `/export` has the same visual shape as the
	// reference TUI while retaining the exact success/error wording.
	if strings.HasPrefix(low, "conversation exported to:") ||
		strings.HasPrefix(low, "conversation copied to clipboard") {
		return "command-result"
	}

	// success markers — "X: did the thing" lines.
	successKeywords := []string{
		"set to ", "set to:", "saved", "added: ", "removed: ", "synced", "renamed",
		"tagged: ", "switched", "applied", "exported", "branched",
		"(allowed)", "(yes)", "title set",
	}
	for _, kw := range successKeywords {
		if strings.Contains(low, kw) {
			return "success"
		}
	}

	// "/<setting>: <value>" prefix — covers cmdEffort / cmdTheme /
	// cmdModel / cmdVim / cmdQuick confirmation strings like
	// "effort: high — small budget".
	settingPrefixes := []string{
		"effort: ", "theme: ", "model: ", "provider: ", "vim mode: ", "quick output",
	}
	for _, p := range settingPrefixes {
		if strings.HasPrefix(low, p) {
			return "success"
		}
	}

	return "info"
}

// isRenderedInfoBox recognizes renderInfoBox output after removing ANSI
// styling. Bordered panels carry their own visual status and should not be
// prefixed with a glyph whose ambiguous terminal width can skew only row one.
func isRenderedInfoBox(out string) bool {
	plain := strings.TrimSpace(xansi.Strip(out))
	return strings.HasPrefix(plain, "╭") && strings.HasSuffix(plain, "╯")
}
