package mcp_tools

import (
	"strings"
	"testing"
)

// TestTruncateDescription_UnderCap — descriptions short enough are
// returned verbatim with no marker.
func TestTruncateDescription_UnderCap(t *testing.T) {
	in := "Run a shell command"
	if got := truncateDescription(in); got != in {
		t.Errorf("short desc should pass through; got %q", got)
	}
}

// TestTruncateDescription_OverCap — long descriptions get clipped
// with the truncation marker so callers / the model can tell content
// was elided. Common case: an OpenAPI-generated MCP server dumping
// 30 KB of endpoint docs into one tool's description.
func TestTruncateDescription_OverCap(t *testing.T) {
	huge := strings.Repeat("a", maxToolDescriptionBytes+5000)
	out := truncateDescription(huge)
	if len(out) > maxToolDescriptionBytes+64 {
		t.Errorf("truncated string still too long (%d bytes)", len(out))
	}
	if !strings.HasSuffix(out, "… (truncated)") {
		t.Errorf("missing truncation marker; tail=%q", out[len(out)-30:])
	}
}

// TestTruncateDescription_UTF8Safe — a description containing
// multi-byte runes near the cap boundary mustn't slice mid-rune.
func TestTruncateDescription_UTF8Safe(t *testing.T) {
	// Build a string that's exactly maxToolDescriptionBytes+30 bytes
	// where the byte-position cap would land mid-rune. Use 3-byte
	// CJK chars (你 = e4 bd a0) to force the boundary problem.
	var b strings.Builder
	for b.Len() < maxToolDescriptionBytes+30 {
		b.WriteString("你好")
	}
	out := truncateDescription(b.String())
	// Must end on the marker; if we sliced mid-rune the suffix would
	// include garbage bytes before "…".
	if !strings.HasSuffix(out, "… (truncated)") {
		t.Errorf("missing/garbled truncation marker on UTF-8 input")
	}
	// And the prefix before the marker must still be valid UTF-8.
	body := strings.TrimSuffix(out, "… (truncated)")
	if !isValidUTF8(body) {
		t.Errorf("truncated body contains invalid UTF-8")
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == 0xFFFD && !strings.ContainsRune(s, 0xFFFD) {
			return false
		}
	}
	return true
}
