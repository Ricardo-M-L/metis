package tui

import "strings"

// classifyREPLOutput maps a REPL command's plain-string output to the
// best-fit Message.Role (info / success / warning / error). Phase B
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
	low := strings.ToLower(strings.TrimSpace(out))

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

	// success markers — "X: did the thing" lines.
	successKeywords := []string{
		"set to ", "saved", "added: ", "removed: ", "synced", "renamed",
		"tagged: ", "switched", "applied", "exported", "branched",
		"(allowed)", "(yes)", "title set",
	}
	for _, kw := range successKeywords {
		if strings.Contains(low, kw) {
			return "success"
		}
	}

	// "/<setting>: <value>" prefix — covers cmdEffort / cmdTheme /
	// cmdModel / cmdVim / cmdFast confirmation strings like
	// "effort: high — small budget".
	settingPrefixes := []string{
		"effort: ", "theme: ", "model: ", "vim mode: ", "fast mode",
	}
	for _, p := range settingPrefixes {
		if strings.HasPrefix(low, p) {
			return "success"
		}
	}

	return "info"
}
