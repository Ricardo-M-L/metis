package screen

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestBodyScreen_RendersHeaderAndBody — sanity: the modal envelope
// (command stripe at top, body in middle, Esc footer at bottom) wraps
// the supplied pre-rendered body without modification.
func TestBodyScreen_RendersHeaderAndBody(t *testing.T) {
	body := "line one\nline two\nline three"
	s := NewBodyScreen("/cost", body)
	s.Resize(80, 20)

	out := s.View()
	if !strings.Contains(out, "/cost") {
		t.Errorf("missing command stripe '/cost' in output:\n%s", out)
	}
	if !strings.Contains(out, "line one") || !strings.Contains(out, "line three") {
		t.Errorf("body content missing; got:\n%s", out)
	}
	if !strings.Contains(out, "Esc") {
		t.Errorf("missing 'Esc' close hint in footer:\n%s", out)
	}
}

// TestBodyScreen_EscDismisses — Esc / q / Ctrl-C all set Done() = true.
func TestBodyScreen_EscDismisses(t *testing.T) {
	cases := []struct {
		name string
		key  tea.KeyMsg
	}{
		{"Esc", tea.KeyPressMsg{Code: tea.KeyEsc}},
		{"q", tea.KeyPressMsg{Code: 'q', Text: "q"}},
		{"Ctrl-C", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewBodyScreen("/help", "body")
			s.Resize(80, 20)
			next, _ := s.Update(tc.key)
			if !next.Done() {
				t.Errorf("%s should dismiss the screen", tc.name)
			}
		})
	}
}

// TestBodyScreen_Scrolls — long content scrolls with ↑↓ / PgUp/PgDn /
// j/k / g/G / mouse wheel. Pin all the binds in one test.
func TestBodyScreen_Scrolls(t *testing.T) {
	// 50 lines of body, 10-line viewport.
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("line ")
		b.WriteString(string(rune('A' + (i % 26))))
		b.WriteString("\n")
	}
	s := NewBodyScreen("/help", b.String())
	s.Resize(80, 14) // bodyHeight = 14 - 4 = 10

	if s.scroll != 0 {
		t.Fatalf("initial scroll = %d, want 0", s.scroll)
	}

	// ↓ once → scroll = 1
	s.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if s.scroll != 1 {
		t.Errorf("KeyDown: scroll = %d, want 1", s.scroll)
	}

	// PgDn → scroll += bodyHeight/2 = 5 → 6
	s.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if s.scroll != 6 {
		t.Errorf("PgDn: scroll = %d, want 6", s.scroll)
	}

	// End → clamped to maxScroll = len(lines) - bodyHeight.
	// 50 lines + trailing newline → 51 split entries → 41.
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if s.scroll != 41 {
		t.Errorf("End: scroll = %d, want 41 (clamped)", s.scroll)
	}

	// Home → 0
	s.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	if s.scroll != 0 {
		t.Errorf("Home: scroll = %d, want 0", s.scroll)
	}

	// j → 1
	s.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if s.scroll != 1 {
		t.Errorf("j: scroll = %d, want 1", s.scroll)
	}

	// k → 0
	s.Update(tea.KeyPressMsg{Code: 'k', Text: "k"})
	if s.scroll != 0 {
		t.Errorf("k: scroll = %d, want 0", s.scroll)
	}

	// G → maxScroll (= 41 with trailing newline)
	s.Update(tea.KeyPressMsg{Code: 'G', Text: "G"})
	if s.scroll != 41 {
		t.Errorf("G: scroll = %d, want 41", s.scroll)
	}

	// g → 0
	s.Update(tea.KeyPressMsg{Code: 'g', Text: "g"})
	if s.scroll != 0 {
		t.Errorf("g: scroll = %d, want 0", s.scroll)
	}

	// Mouse wheel down → 1
	s.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if s.scroll != 1 {
		t.Errorf("WheelDown: scroll = %d, want 1", s.scroll)
	}
}

// TestBodyScreen_FooterHintAdaptive — when content fits in viewport,
// footer omits the scroll keybinds (only "Esc to close" remains).
func TestBodyScreen_FooterHintAdaptive(t *testing.T) {
	// Short body, big viewport — no scroll needed.
	short := NewBodyScreen("/version", "0.1.1")
	short.Resize(80, 20)
	out := short.View()
	if strings.Contains(out, "PgUp") || strings.Contains(out, "PgDn") {
		t.Errorf("short body should hide scroll keybinds; got footer with PgUp/PgDn:\n%s", out)
	}

	// Long body, small viewport — scroll keybinds appear.
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("filler line\n")
	}
	long := NewBodyScreen("/help", b.String())
	long.Resize(80, 10)
	out = long.View()
	if !strings.Contains(out, "PgUp") {
		t.Errorf("long body should show scroll keybinds; got:\n%s", out)
	}
}
