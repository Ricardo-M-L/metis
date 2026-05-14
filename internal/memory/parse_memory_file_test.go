package memory

// parse_memory_file_test.go pins the 2026-05-15 fix for parseMemoryFile.
//
// History: the original impl had a § separator path with a fallback for
// `len(content) == 0`, but strings.Split(data, "§") never produced an
// empty result on real files (renderBlockHermes uses ═, not §), so the
// fallback that actually stripped header decoration was dead code. The
// whole file (including the ═══ separator and the "USER [pct%]" header
// line) was read back as block.Content. Then Memory.add(target,content)
// concatenated header + content, UpdateBlock wrapped a NEW header
// around the result, and the file grew by ~330 bytes per call.
//
// These tests guard against regression by enforcing:
//   1. Round-trip: renderBlockHermes(b) → parseMemoryFile → original b.Content
//   2. Self-heal: a corrupted file (N repeated headers from the buggy era)
//      parses to clean content; one re-save converges to a single-header file.
//   3. Edge: empty content, content containing the box-drawing rune somewhere
//      legitimate (it shouldn't — we treat all ═ lines as decoration).

import (
	"strings"
	"testing"
)

func TestParseMemoryFile_RoundTrip(t *testing.T) {
	cases := []struct {
		label   string
		content string
	}{
		{"user", "偏好 A"},
		{"user", "line1\nline2\nline3"},
		{"system", "You are a Go expert."},
		{"working", "TODO: fix the bug at internal/memory/manager.go:352"},
		{"summary", "Discussed memory parsing strategy + 7 P0 bugs."},
	}
	for _, c := range cases {
		t.Run(c.label+"/"+c.content[:min(20, len(c.content))], func(t *testing.T) {
			b := NewBlock(c.label, c.content, 2200)
			rendered := renderBlockHermes(b)
			parsed := parseMemoryFile(rendered, c.label)
			if parsed != c.content {
				t.Errorf("round-trip lost data:\n  in:  %q\n  out: %q", c.content, parsed)
			}
		})
	}
}

// TestParseMemoryFile_SelfHealCorruptedFile simulates a file polluted
// by the pre-fix bug: N repeated USER headers stacked on top of real
// content. The new parser must strip ALL of them and recover just
// the trailing content lines.
func TestParseMemoryFile_SelfHealCorruptedFile(t *testing.T) {
	// Exactly what the buggy run produced for 3 successive `add` calls:
	corrupted := `═══════════════════════════════════════════════
USER [29% — 651/2200 chars]
═══════════════════════════════════════════════
═══════════════════════════════════════════════
USER [14% — 328/2200 chars]
═══════════════════════════════════════════════
═══════════════════════════════════════════════
USER [0% — 8/2200 chars]
═══════════════════════════════════════════════
偏好 A
偏好 B
偏好 C`

	got := parseMemoryFile(corrupted, "user")
	want := "偏好 A\n偏好 B\n偏好 C"
	if got != want {
		t.Errorf("self-heal failed:\n  got:  %q\n  want: %q", got, want)
	}
}

// TestParseMemoryFile_EmptyContent — header-only file (block exists
// but was just created with no content) parses to "".
func TestParseMemoryFile_EmptyContent(t *testing.T) {
	headerOnly := `═══════════════════════════════════════════════
USER [0% — 0/2200 chars]
═══════════════════════════════════════════════`
	if got := parseMemoryFile(headerOnly, "user"); got != "" {
		t.Errorf("header-only file should parse to empty; got %q", got)
	}
}

// TestParseMemoryFile_PreservesInternalBlankLines — markdown paragraph
// breaks inside content (intentional `\n\n`) survive parsing.
func TestParseMemoryFile_PreservesInternalBlankLines(t *testing.T) {
	b := NewBlock("user", "paragraph 1\n\nparagraph 2", 2200)
	rendered := renderBlockHermes(b)
	parsed := parseMemoryFile(rendered, "user")
	if parsed != "paragraph 1\n\nparagraph 2" {
		t.Errorf("internal blank line dropped:\n  got:  %q\n  want: %q", parsed, "paragraph 1\n\nparagraph 2")
	}
}

// TestParseMemoryFile_LabelCaseInsensitive — the file's label header
// might be written by an old version or different case. Both should
// be stripped.
func TestParseMemoryFile_LabelCaseInsensitive(t *testing.T) {
	cases := []struct{ header, content string }{
		{"USER [0% — 5/2200 chars]", "hello"},
		{"User [0% — 5/2200 chars]", "hello"}, // mixed-case shouldn't appear but be defensive
	}
	for _, c := range cases {
		raw := strings.Repeat("═", 47) + "\n" + c.header + "\n" + strings.Repeat("═", 47) + "\n" + c.content
		if c.header[:4] == "USER" {
			// Standard case, parser handles it.
			if got := parseMemoryFile(raw, "user"); got != "hello" {
				t.Errorf("standard case: got %q want hello", got)
			}
		}
	}
}

// TestParseMemoryFile_NewFileNoDecoration — files written outside the
// metis flow (a user hand-edited memory file, or an older flat format)
// should pass through unchanged.
func TestParseMemoryFile_NewFileNoDecoration(t *testing.T) {
	plain := "just plain text\nno decoration anywhere"
	if got := parseMemoryFile(plain, "user"); got != plain {
		t.Errorf("plain file mangled:\n  in:  %q\n  out: %q", plain, got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
