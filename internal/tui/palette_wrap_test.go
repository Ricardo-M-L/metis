package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestPalette_DownWrapsToTop — claude-code parity. Pre-fix: ↓ at the
// last entry was a no-op (only `if palCursor < len-1`). Post-fix: it
// wraps to index 0.
func TestPalette_DownWrapsToTop(t *testing.T) {
	m := newSlashTestModel(t)
	// Trigger palette open by typing "/" — seeds palMatched with all
	// registered commands.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if !m.showPalette {
		t.Fatalf("palette did not open on '/'")
	}
	if len(m.palMatched) < 2 {
		t.Fatalf("need ≥2 matches to test wrap; got %d", len(m.palMatched))
	}

	// Park cursor on the last entry, then press ↓ — should wrap to 0.
	m.palCursor = len(m.palMatched) - 1
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.palCursor != 0 {
		t.Errorf("KeyDown at last index (%d) should wrap to 0; got %d",
			len(m.palMatched)-1, m.palCursor)
	}
}

// TestPalette_UpWrapsToBottom — symmetric: ↑ at index 0 wraps to last.
func TestPalette_UpWrapsToBottom(t *testing.T) {
	m := newSlashTestModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if len(m.palMatched) < 2 {
		t.Fatalf("need ≥2 matches to test wrap; got %d", len(m.palMatched))
	}

	m.palCursor = 0
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	want := len(m.palMatched) - 1
	if m.palCursor != want {
		t.Errorf("KeyUp at index 0 should wrap to %d; got %d", want, m.palCursor)
	}
}

// TestPalette_DownNormalAdvance — wrap doesn't break the common case.
// At index N (N < len-1), ↓ advances to N+1.
func TestPalette_DownNormalAdvance(t *testing.T) {
	m := newSlashTestModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if len(m.palMatched) < 3 {
		t.Fatalf("need ≥3 matches; got %d", len(m.palMatched))
	}

	m.palCursor = 1
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.palCursor != 2 {
		t.Errorf("KeyDown from index 1 should go to 2; got %d", m.palCursor)
	}
}

// TestPalette_UpNormalAdvance — at index N (N > 0), ↑ goes to N-1.
func TestPalette_UpNormalAdvance(t *testing.T) {
	m := newSlashTestModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if len(m.palMatched) < 3 {
		t.Fatalf("need ≥3 matches; got %d", len(m.palMatched))
	}

	m.palCursor = 2
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.palCursor != 1 {
		t.Errorf("KeyUp from index 2 should go to 1; got %d", m.palCursor)
	}
}

// TestPalette_DownSingleMatch — defensive: with exactly one match,
// both ↓ and ↑ keep cursor at 0 (modular arithmetic boundary).
func TestPalette_DownSingleMatch(t *testing.T) {
	m := newSlashTestModel(t)
	for _, r := range "/effort" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	// /effort is unique enough that the prefix match is exactly 1
	// (or close to 1). Just verify single-match case doesn't crash.
	if len(m.palMatched) == 1 {
		m.Update(tea.KeyMsg{Type: tea.KeyDown})
		if m.palCursor != 0 {
			t.Errorf("KeyDown with single match should stay at 0; got %d", m.palCursor)
		}
		m.Update(tea.KeyMsg{Type: tea.KeyUp})
		if m.palCursor != 0 {
			t.Errorf("KeyUp with single match should stay at 0; got %d", m.palCursor)
		}
	}
}
