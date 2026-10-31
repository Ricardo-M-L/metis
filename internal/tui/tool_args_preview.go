package tui

// tool_args_preview.go — extract a one-liner from a partial tool-args
// JSON buffer for the spinner subline (T12). Inspired by kimi-cli's
// streamingjson, but we don't actually need to parse — we just want
// to show "Read · /tmp/foo..." while the LLM types and the args
// haven't closed yet.

import (
	"strings"
	"unicode"
)

// previewStreamingArgs returns a short, human-friendly hint of the
// in-flight tool args. Strategy:
//
//   - look for the first "key":"value" pair
//   - if found, render: tool · value
//   - if not yet found (still typing the key, or value is non-string),
//     fall back to the raw chunk so the user sees *something* moving
//
// Bounds the displayed preview while leaving layout to the status renderer.
func previewStreamingArgs(toolName string, raw []byte) string {
	if len(raw) == 0 {
		return toolName
	}
	str := string(raw)

	// Try: first quoted string after the first colon. That's the
	// most common shape: {"path":"X", "key":"Y"} → first value X.
	if val, ok := extractFirstStringValue(str); ok {
		val = streamingArgsSnippet(val)
		if val == "" {
			return toolName
		}
		if toolName == "" {
			return val
		}
		return toolName + " · " + val
	}

	// Fallback: whatever's typed so far, with the leading `{` and
	// any whitespace trimmed. Useful when the LLM is still typing
	// the key half of the first pair.
	preview := strings.TrimLeftFunc(str, func(r rune) bool {
		return r == '{' || unicode.IsSpace(r)
	})
	preview = streamingArgsSnippet(preview)
	if preview == "" {
		return toolName
	}
	if toolName == "" {
		return preview
	}
	return toolName + " · " + preview
}

// Leave ordinary paths and descriptions intact for the cell-aware status
// renderer. This defensive cap only bounds pathological streamed arguments;
// an ellipsis means content was actually omitted, not merely still arriving.
func streamingArgsSnippet(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(toolArgsDisplayText(value), "\n", " "))
	return truncateMiddleCells(value, toolArgsPreviewMaxRunes)
}

// extractFirstStringValue scans s for the first `"key":"value"`
// pattern and returns value (without quotes). Returns ("", false) if
// no such pair has been typed yet.
//
// Hand-rolled because encoding/json refuses to parse partial input.
// We don't try to handle escape sequences perfectly — a trailing
// `\` is just dropped, which is correct UX (the next chunk will
// complete it). If the LLM emits a non-string first value (number,
// nested object), we return false and the caller falls back to raw.
func extractFirstStringValue(s string) (string, bool) {
	colon := strings.Index(s, ":")
	if colon < 0 {
		return "", false
	}
	rest := s[colon+1:]
	// Skip whitespace after colon.
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	if !strings.HasPrefix(rest, `"`) {
		return "", false
	}
	rest = rest[1:] // consume opening quote
	end := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] == '\\' {
			// Skip the escaped char if present.
			i++
			continue
		}
		if rest[i] == '"' {
			end = i
			break
		}
	}
	if end < 0 {
		// Not yet closed — show what we have.
		return rest, true
	}
	return rest[:end], true
}
