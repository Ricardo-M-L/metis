package tui

// agent_color_test.go — locks the G.7 (2026-05-12) per-teammate color
// palette contract:
//
//   1. "general" / "main" / "" → uncolored (nil Color).
//   2. Any other name → a deterministic palette color (same name →
//      same color across runs).
//   3. Two different names hash to two different palette indices most
//      of the time (statistical — verify with a handful of expected
//      pairs we KNOW differ).
//   4. StyleForAgent returns a usable Style in both cases.

import (
	"testing"
)

func TestColorForAgent_UncoloredNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "general", "main"} {
		if c := ColorForAgent(name); c != nil {
			t.Errorf("ColorForAgent(%q) = %v, want nil", name, c)
		}
	}
}

func TestColorForAgent_DeterministicAndPaletteBound(t *testing.T) {
	t.Parallel()
	// Same name → same color, always.
	first := ColorForAgent("alice")
	if first == nil {
		t.Fatal("alice should have a non-nil color")
	}
	for i := 0; i < 50; i++ {
		again := ColorForAgent("alice")
		if again != first {
			t.Fatalf("ColorForAgent(\"alice\") non-deterministic: %v vs %v", first, again)
		}
	}
}

func TestColorForAgent_DifferentNamesUsePaletteRange(t *testing.T) {
	t.Parallel()
	// Use a wide enough sample that we'd be very unlucky to all hash
	// into the same palette index (8 slots, 32 names → ~99% chance
	// of at least 4 distinct colors used).
	names := []string{
		"alice", "bob", "carol", "dave", "erin", "frank",
		"grace", "harry", "iris", "jack", "kelly", "lou",
		"marie", "nate", "olive", "paul", "quinn", "ron",
		"sara", "tom", "uma", "vic", "wade", "xena",
		"yara", "zane", "aaron", "beth", "cara", "dora",
		"eric", "fern",
	}
	seen := make(map[any]bool)
	for _, n := range names {
		c := ColorForAgent(n)
		if c == nil {
			t.Errorf("name %q got nil color (only general/main/'' should be uncolored)", n)
			continue
		}
		seen[c] = true
	}
	if len(seen) < 4 {
		t.Errorf("only %d distinct palette colors used across %d names — palette appears under-used", len(seen), len(names))
	}
}

func TestStyleForAgent_UncoloredReturnsEmptyStyle(t *testing.T) {
	t.Parallel()
	// "general" → uncolored — style should be the default (no
	// foreground set). We can't directly inspect Foreground via the
	// public API, but Render of empty input should at least not panic
	// and produce empty output for empty input.
	st := StyleForAgent("general")
	out := st.Render("")
	if out != "" {
		t.Errorf("uncolored style with empty input should render empty; got %q", out)
	}
}

func TestStyleForAgent_ColoredRendersNonEmpty(t *testing.T) {
	t.Parallel()
	st := StyleForAgent("alice")
	out := st.Render("[alice]")
	if out == "" {
		t.Error("colored style should not render to empty")
	}
	// The rendered string includes ANSI escape codes when colored.
	// We don't pin a specific escape because lipgloss may differ by
	// terminal profile — just confirm "[alice]" appears verbatim.
	if !contains(out, "[alice]") {
		t.Errorf("rendered output should contain the label; got %q", out)
	}
}

// contains is a tiny strings.Contains shim that avoids dragging
// strings into a one-liner test.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
