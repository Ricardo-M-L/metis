package tui

import (
	"strings"
	"testing"
)

func TestPalette_ClaudeCodeBorderlessSixRowWindow(t *testing.T) {
	m := newSlashTestModel(t)
	m.width = 100
	m.showPalette = true
	m.palFilter = ""
	m.matchCommands()
	if len(m.palMatched) <= paletteMaxRows {
		t.Fatalf("fixture needs more than %d commands, got %d", paletteMaxRows, len(m.palMatched))
	}
	m.palCursor = len(m.palMatched) / 2

	out := stripANSI(renderPalette(m))
	for _, legacyChrome := range []string{"┌", "└", "│", "▸", " more ("} {
		if strings.Contains(out, legacyChrome) {
			t.Fatalf("palette retained pre-Claude-Code chrome %q:\n%s", legacyChrome, out)
		}
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != paletteMaxRows {
		t.Fatalf("visible rows=%d, want Claude Code's %d:\n%s", len(lines), paletteMaxRows, out)
	}
	selected := "/" + m.palMatched[m.palCursor].Name
	if !strings.Contains(out, selected) {
		t.Fatalf("centered window lost selected command %q:\n%s", selected, out)
	}

	start := m.palCursor - paletteMaxRows/2
	if start < 0 {
		start = 0
	}
	if maxStart := len(m.palMatched) - paletteMaxRows; start > maxStart {
		start = maxStart
	}
	if first := "/" + m.palMatched[start].Name; !strings.Contains(lines[0], first) {
		t.Fatalf("first visible row=%q, want centered start %q", lines[0], first)
	}
}

func TestPalette_NoMatchesRendersNothing(t *testing.T) {
	m := newSlashTestModel(t)
	m.showPalette = true
	m.palFilter = "definitely-no-such-command"
	m.matchCommands()
	if got := renderPalette(m); got != "" {
		t.Fatalf("Claude Code hides an empty suggestion list; got %q", stripANSI(got))
	}
}
