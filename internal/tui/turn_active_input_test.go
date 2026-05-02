package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestTurnActive_AcceptsTyping — pre-fix: handleKey gated all keys
// behind `if m.turnActive { ... return m, nil }`, so the user couldn't
// type the next prompt while the assistant was generating. Post-fix:
// keystrokes flow through to the textarea; only the SUBMIT path
// (handleSubmit) blocks, surfacing a hint instead of double-spawning
// a turn. Mirrors claude-code's behavior.
func TestTurnActive_AcceptsTyping(t *testing.T) {
	m := newSlashTestModel(t)
	m.turnActive = true // simulate in-flight assistant generation

	// Send a few keystrokes — the user typing the next prompt while
	// the model is still streaming the current one.
	for _, r := range "hello" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	got := m.input.Value()
	if got != "hello" {
		t.Errorf("turnActive should NOT eat keystrokes; input.Value() = %q, want %q", got, "hello")
	}
}

// TestTurnActive_BlocksSubmit — defensive: the input freeze removal
// must not regress submit-blocking. Pressing Enter while a turn is in
// flight should still surface the hint and NOT spawn a second turn.
func TestTurnActive_BlocksSubmit(t *testing.T) {
	m := newSlashTestModel(t)
	for _, r := range "hello" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.turnActive = true
	beforeMsgs := len(m.messages)

	pressEnter(t, m)

	// Look for the "(turn still running ...)" hint in messages.
	found := false
	for i := beforeMsgs; i < len(m.messages); i++ {
		if strings.Contains(m.messages[i].Content, "turn still running") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Enter while turnActive should surface 'turn still running' hint; got messages[%d:]=%+v",
			beforeMsgs, m.messages[beforeMsgs:])
	}
	// Input should be preserved (user's prompt not lost).
	if m.input.Value() != "hello" {
		t.Errorf("input should be preserved when submit blocked; got %q", m.input.Value())
	}
}

// TestTurnActive_AcceptsBackspace — typos must be fixable mid-turn.
// Pre-fix: backspace was eaten by the key gate. Post-fix: it edits
// the textarea normally.
func TestTurnActive_AcceptsBackspace(t *testing.T) {
	m := newSlashTestModel(t)
	for _, r := range "helloo" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.turnActive = true

	m.Update(tea.KeyMsg{Type: tea.KeyBackspace})

	if m.input.Value() != "hello" {
		t.Errorf("backspace should work mid-turn; input.Value() = %q, want %q",
			m.input.Value(), "hello")
	}
}

// TestTurnActive_MouseWheelScrolls — even with a turn in flight, mouse
// wheel must keep scrolling the chat surface. Currently the
// MouseMsg handler in tui_update.go has no turnActive guard, so this
// test pins that behavior in (regression guard if a future patch
// accidentally adds one).
func TestTurnActive_MouseWheelScrolls(t *testing.T) {
	m := newSlashTestModel(t)
	// Stuff the chat with enough items that the list has somewhere to
	// scroll.
	for i := 0; i < 50; i++ {
		m.messages = append(m.messages, Message{
			Role:    "user",
			Content: "filler message " + string(rune('A'+i%26)),
		})
	}
	// Force chat list to ingest the items by triggering a render path
	// that calls SetItemsKeepScroll (via View()).
	_ = m.View()
	m.chatList.ScrollToBottom()
	if !m.chatList.AtBottom() {
		t.Fatalf("precondition: list should be at bottom after ScrollToBottom")
	}

	m.turnActive = true
	// Send wheel-up — this used to silently no-op when turnActive (per
	// the user's bug report). Post-fix: scrolls.
	m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp})

	if m.chatList.AtBottom() {
		t.Errorf("MouseWheelUp during turnActive should leave AtBottom() = false; still at bottom")
	}
}
