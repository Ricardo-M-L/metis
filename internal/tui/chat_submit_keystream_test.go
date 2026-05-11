package tui

// chat_submit_keystream_test.go — rune-by-rune submission tests for
// the chat surface (CHAT-TEATEST, 2026-05-11).
//
// Why this exists: tmux-driven E2E ("tmux send-keys 'hello' Enter")
// kept losing the Enter on a detached pty — bubbletea's batched
// stdin reader + tmux's no-delay key injection race, and Enter
// sometimes arrived before the textarea had registered focus on
// the new chars. We can't fix tmux, but we CAN verify that the
// in-process Update(KeyPressMsg) path works end-to-end — which is
// what bubbletea's real-terminal runtime ultimately calls anyway.
//
// These tests bypass the pty entirely. Each test:
//  1. builds a Model with a fakeProvider via newSlashTestModelWithLoop
//  2. types the prompt one rune at a time via typeRunes
//  3. presses Enter via pressEnter
//  4. asserts on the chat state (messages appended, turn started,
//     /context rendered inline, modal opened for /help, etc.)
//
// Anyone refactoring handleSubmit / handleKey / textarea wiring
// will trip these immediately if the keystream → submit pipe breaks.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// typeRunes injects each rune of `s` as its own KeyPressMsg, exactly
// like a human typing into a real terminal. Text is set on the Key
// struct so textarea sees printable characters (Code alone would be
// interpreted as a special key for non-ASCII runes).
//
// This is the synchronous equivalent of `tmux send-keys -l "hello"`
// — except every keypress is processed by Update() before the next
// one arrives, so there's no race with the runtime's event loop.
func typeRunes(t *testing.T, m *Model, s string) {
	t.Helper()
	for _, r := range s {
		msg := tea.KeyPressMsg{Code: r, Text: string(r)}
		updated, _ := m.Update(msg)
		*m = *(updated.(*Model))
	}
}

// TestChatSubmit_RuneByRuneSubmission — the smoke test that locks the
// failure mode I hit with `tmux send-keys`. Type "hello world", press
// Enter, expect a user-role message in the chat AND the turn flagged
// active. If this regresses, the textarea→handleSubmit→AppendUser
// pipe is broken.
func TestChatSubmit_RuneByRuneSubmission(t *testing.T) {
	m := newSlashTestModelWithLoop(t)
	before := len(m.messages)

	typeRunes(t, m, "hello world")
	if got := m.input.Value(); got != "hello world" {
		t.Fatalf("textarea should hold typed text; got %q", got)
	}
	cmd := pressEnter(t, m)
	if cmd == nil {
		t.Errorf("submitting non-empty text should produce a Cmd to drive the turn")
	}

	// A "user" message should now be in the chat scroll. The previous
	// message added by newSlashTestModelWithLoop is an "info" row, so
	// we walk from the end looking for the user submission.
	found := false
	for i := len(m.messages) - 1; i >= before; i-- {
		if m.messages[i].Role == "user" && strings.Contains(m.messages[i].Content, "hello world") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("submit should append a user-role message containing 'hello world'; got:\n%s", dumpMessages(m))
	}
	// turn-active flag drives the spinner; without this the UI looks
	// idle while the goroutine runs.
	if !m.turnActive {
		t.Errorf("turnActive must flip true after submit so spinner / queue-input handling fire")
	}
	// Input must clear after submit so the user can type the next
	// message without first manually erasing the previous.
	if m.input.Value() != "" {
		t.Errorf("input should clear after submit; got %q", m.input.Value())
	}
}

// TestChatSubmit_RuneByRuneSlashContext — the claude-code-parity
// 2026-05-11 contract: typing /context one rune at a time, then
// Enter, must produce an inline info-role message (NOT open a modal).
// This is the regression the user explicitly asked us to lock when
// they said "I don't want Esc-to-close for /context."
func TestChatSubmit_RuneByRuneSlashContext(t *testing.T) {
	m := newSlashTestModelWithLoop(t)
	before := len(m.messages)

	typeRunes(t, m, "/context")
	pressEnter(t, m)

	if m.activeScreen != nil {
		t.Errorf("/context typed rune-by-rune must NOT open a modal screen; got %T", m.activeScreen)
	}
	if len(m.messages) <= before {
		t.Fatalf("/context should append at least one inline message; before=%d after=%d", before, len(m.messages))
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != "info" {
		t.Errorf("/context output should be info-role; got role=%q", last.Role)
	}
	if !strings.Contains(last.Content, "Context Usage") {
		t.Errorf("/context content should carry the 'Context Usage' header; got: %q", truncForLog(last.Content))
	}
}

// TestChatSubmit_RuneByRuneSlashHelp — counter-case: /help SHOULD
// open the modal. Pin the modal-vs-inline distinction so a refactor
// can't accidentally collapse both into one path.
func TestChatSubmit_RuneByRuneSlashHelp(t *testing.T) {
	m := newSlashTestModelWithLoop(t)
	typeRunes(t, m, "/help")
	pressEnter(t, m)
	if m.activeScreen == nil {
		t.Errorf("/help should open a modal BodyScreen; got nil activeScreen")
	}
}

// TestChatSubmit_EnterOnEmpty — Enter with no text typed should be a
// no-op. Otherwise users hitting Enter to confirm a paste / clear a
// confused state would accidentally submit an empty turn.
func TestChatSubmit_EnterOnEmpty(t *testing.T) {
	m := newSlashTestModelWithLoop(t)
	before := len(m.messages)
	turnBefore := m.turnActive

	cmd := pressEnter(t, m)
	if cmd != nil {
		t.Errorf("Enter on empty input should not produce a turn-driving Cmd; got %T", cmd)
	}
	if len(m.messages) != before {
		t.Errorf("Enter on empty input must not append messages; before=%d after=%d", before, len(m.messages))
	}
	if m.turnActive != turnBefore {
		t.Errorf("Enter on empty input must not flip turnActive; before=%v after=%v", turnBefore, m.turnActive)
	}
}

// TestChatSubmit_RuneByRuneMultibyte — type CJK characters one rune
// at a time. Earlier tmux runs lost multibyte sequences because
// terminal stdin readers can partial-read mid-rune; the in-process
// Update path doesn't have that failure mode and the text should
// round-trip exactly.
func TestChatSubmit_RuneByRuneMultibyte(t *testing.T) {
	m := newSlashTestModelWithLoop(t)
	prompt := "你好世界"
	typeRunes(t, m, prompt)
	if got := m.input.Value(); got != prompt {
		t.Errorf("multibyte text round-trip failed: got %q want %q", got, prompt)
	}
	pressEnter(t, m)
	found := false
	for _, msg := range m.messages {
		if msg.Role == "user" && strings.Contains(msg.Content, prompt) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("CJK submit not visible in transcript; got:\n%s", dumpMessages(m))
	}
}

// TestChatSubmit_MultiTurnSequence — type → submit → type → submit.
// Catches state-not-reset bugs: if turnActive doesn't drop back to
// false between turns (or if input doesn't clear properly), the
// second submit would silently fail. This is the exact failure mode
// I hit with tmux on the second /context invocation: first one
// worked, second was dropped.
func TestChatSubmit_MultiTurnSequence(t *testing.T) {
	m := newSlashTestModelWithLoop(t)
	// First submission — uses a SLASH command so we don't need to
	// drive the fakeProvider's turn lifecycle to clear turnActive.
	typeRunes(t, m, "/context")
	pressEnter(t, m)
	if m.input.Value() != "" {
		t.Fatalf("input should clear after first /context; got %q", m.input.Value())
	}
	beforeSecond := len(m.messages)
	// Second submission — must also reach the renderer.
	typeRunes(t, m, "/context")
	pressEnter(t, m)
	if len(m.messages) <= beforeSecond {
		t.Errorf("second /context must also append; before=%d after=%d (state leak between submits?)",
			beforeSecond, len(m.messages))
	}
}

// dumpMessages renders the message slice as a labelled list for
// failure output. Keeps test diagnostics readable when the assertion
// fires.
func dumpMessages(m *Model) string {
	var b strings.Builder
	for i, msg := range m.messages {
		body := msg.Content
		if len(body) > 200 {
			body = body[:200] + "…"
		}
		b.WriteString("  [")
		b.WriteString(msg.Role)
		b.WriteString("] ")
		b.WriteString(body)
		b.WriteByte('\n')
		_ = i
	}
	return b.String()
}

// truncForLog clips a string for failure output so a 30 KB /context
// dump doesn't bury the actual error message.
func truncForLog(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
