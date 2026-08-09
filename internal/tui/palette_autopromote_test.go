package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestPalette_EnterAutopromotesIncompleteSlash — claude-code parity.
// Pre-fix: typing "/effo" + Enter dispatched literally /effo, which
// failed slash.Registry.Get and surfaced as "unknown: /effo — try /help"
// even though the palette below the input was showing /effort
// highlighted. Post-fix: Enter promotes the typed buffer to the
// palette's highlighted candidate before submitting.
//
// Test drives Model.Update with the same keystrokes the user types:
// "/", "e", "f", "f", "o", Enter — then asserts no "unknown:" message
// landed and that the message log contains evidence of /effort running.
func TestPalette_EnterAutopromotesIncompleteSlash(t *testing.T) {
	m := newSlashTestModel(t)

	// Type "/effo" — palette opens on the leading "/" and matchCommands
	// runs after each keystroke (handlePaletteKey path).
	for _, r := range "/effo" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	if !m.showPalette {
		t.Fatalf("palette should be open after typing /effo")
	}
	if len(m.palMatched) == 0 {
		t.Fatalf("palette should have at least one match for /effo")
	}
	if m.palMatched[0].Name != "effort" {
		t.Errorf("first palette match for /effo should be 'effort', got %q", m.palMatched[0].Name)
	}

	pressEnter(t, m)

	// Pre-fix would have logged: "unknown: /effo — try /help"
	for _, msg := range m.messages {
		if strings.Contains(msg.Content, "unknown: /effo") {
			t.Fatalf("autopromote regression: still got unknown error: %q", msg.Content)
		}
		if strings.Contains(msg.Content, "unknown: /") {
			t.Fatalf("autopromote regression: got unknown error: %q", msg.Content)
		}
	}
	// Bare /effort opens the inline slider rather than a full-window screen.
	var output string
	if m.effortPicker != nil {
		output = m.effortPicker.InlineView()
	} else if m.activeScreen != nil {
		output = m.activeScreen.View()
	} else {
		var b strings.Builder
		for _, msg := range m.messages {
			b.WriteString(msg.Content + "\n")
		}
		output = b.String()
	}
	if !strings.Contains(output, "Faster") && !strings.Contains(output, "Smarter") &&
		!strings.Contains(strings.ToLower(output), "effort") &&
		!strings.Contains(output, "Reasoning") {
		t.Errorf("expected /effort widget or output in message log; got: %+v", messageContents(m))
	}
}

// TestPalette_EnterPreservesArgs — when the typed text has args
// ("/eff high"), the autopromote must keep the args intact:
// "/effort high" rather than dropping " high".
func TestPalette_EnterPreservesArgs(t *testing.T) {
	m := newSlashTestModel(t)
	for _, r := range "/eff high" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	pressEnter(t, m)

	for _, msg := range m.messages {
		if strings.Contains(msg.Content, "unknown:") {
			t.Fatalf("got unknown error after /eff high: %q", msg.Content)
		}
	}
	// /effort with arg "high" sets effort to high — handler reports
	// something like "effort: high" or "Reasoning Effort: high".
	found := false
	for _, msg := range m.messages {
		if strings.Contains(strings.ToLower(msg.Content), "high") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'high' in /effort output (args preserved); got: %+v", messageContents(m))
	}
}

// A short prefix such as /s matches many real commands (save, sessions,
// share, skills, stats, status, ...). Enter must not pick whichever command
// happens to be first in registration order. The user can still choose a row
// with Tab/arrow, which writes an exact command into the input before Enter.
func TestPalette_EnterDoesNotPromoteAmbiguousPrefix(t *testing.T) {
	m := newSlashTestModel(t)
	for _, r := range "/s" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if len(m.palMatched) < 2 {
		t.Fatalf("fixture needs an ambiguous /s prefix, matches=%+v", m.palMatched)
	}
	if got := m.promotePaletteSelection("/s"); got != "/s" {
		t.Fatalf("ambiguous /s promoted to %q; want original input", got)
	}
}

// TestPalette_EnterDoesNotPromoteExactMatch — defensive: when the typed
// name is itself a valid command (/help), Enter must dispatch /help
// even if the palette cursor happened to land on /history. Otherwise
// the palette becomes a footgun: the user can't type a command they
// know exists without first dismissing the palette.
func TestPalette_EnterDoesNotPromoteExactMatch(t *testing.T) {
	m := newSlashTestModel(t)
	for _, r := range "/help" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if !m.showPalette {
		t.Fatalf("palette should be open after /help")
	}
	// Manually move the cursor to point at something OTHER than the
	// exact /help match, so we can prove autopromote skipped it.
	for i, c := range m.palMatched {
		if c.Name == "history" {
			m.palCursor = i
			break
		}
	}
	pressEnter(t, m)

	// Phase A migration: /help now opens a BodyScreen modal overlay
	// instead of inlining the giant command listing. Look there first;
	// fall back to messages for any future regression.
	var output string
	if m.activeScreen != nil {
		output = strings.ToLower(m.activeScreen.View())
	} else {
		var b strings.Builder
		for _, msg := range m.messages {
			b.WriteString(msg.Content + "\n")
		}
		output = strings.ToLower(b.String())
	}
	// Phase C2: /help now opens the tabbed HelpScreen. Look for any
	// of: tab labels, shortcut headings, or known command names that
	// would appear in either /help or /history's overlay.
	helpMarkers := []string{"general", "commands", "shortcuts", "metis commands"}
	historyMarkers := []string{"session history"}
	helpMatched := false
	for _, m := range helpMarkers {
		if strings.Contains(output, m) {
			helpMatched = true
			break
		}
	}
	historyMatched := false
	for _, m := range historyMarkers {
		if strings.Contains(output, m) {
			historyMatched = true
			break
		}
	}
	if !helpMatched {
		t.Errorf("exact-match /help should win over palette cursor; got: %+v", messageContents(m))
	}
	if historyMatched {
		t.Errorf("autopromote regression: /history overlay opened instead of /help")
	}
}

// messageContents extracts message bodies for diagnostic output.
func messageContents(m *Model) []string {
	out := make([]string, 0, len(m.messages))
	for _, msg := range m.messages {
		c := msg.Content
		if len(c) > 80 {
			c = c[:77] + "..."
		}
		out = append(out, msg.Role+": "+c)
	}
	return out
}
