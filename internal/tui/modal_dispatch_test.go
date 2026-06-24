package tui

import (
	"strings"
	"testing"
)

// TestModalDispatch_REPLCommand — typing a modal-class REPL command
// (e.g. /help, /cost, /tokens, /env) opens a BodyScreen overlay rather
// than appending the output inline to chat. Pin this for the 19 entries
// in modalCommands so a future refactor of handleSubmit doesn't quietly
// regress to the inline path.
func TestModalDispatch_REPLCommand(t *testing.T) {
	cases := []string{
		"help",
		"doctor",
		// "context" (2026-05-11, image #1) and "cost"/"tokens" (2026-06-24,
		// image #4) intentionally removed from this list — they now render
		// inline as chat messages (claude-code parity) so the conversation
		// stays visible and the user doesn't press Esc to return to typing.
		// Inline coverage is in TestInfoCommandsRenderInline below.
		"env",
		"version",
	}
	for _, name := range cases {
		t.Run("/"+name, func(t *testing.T) {
			m := newSlashTestModel(t)
			before := len(m.messages)

			// Type the slash command and submit.
			m.input.SetValue("/" + name)
			pressEnter(t, m)

			if m.activeScreen == nil {
				t.Errorf("/%s did not open a screen overlay (activeScreen is nil); appended %d messages",
					name, len(m.messages)-before)
			}
		})
	}
}

// TestModalDispatch_NonModalStaysInline — counter-example: short status
// commands (/title, /save, /tag etc.) still go inline (no full-screen
// modal for a one-line confirmation). Only browseable content opens
// the BodyScreen.
func TestModalDispatch_NonModalStaysInline(t *testing.T) {
	cases := []string{
		"/title some sprint",
		"/save",
		"/tag scratch",
		"/add-dir /tmp",
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			m := newSlashTestModel(t)
			m.input.SetValue(input)
			pressEnter(t, m)

			if m.activeScreen != nil {
				t.Errorf("%s should NOT open a screen overlay (quick confirmation, not browseable content)", input)
			}
			// Verify *something* came back to the user (either a confirmation
			// or a "no session store" hint, depending on test fixture state).
			if len(m.messages) <= 1 { // 1 = the "(test session)" pre-seed
				t.Errorf("%s should append at least one inline message; got: %+v", input, messageContents(m))
			}
		})
	}
}

// TestModalDispatch_BodyScreenContent — verify the BodyScreen actually
// carries the rendered body, not just an empty modal envelope.
func TestModalDispatch_BodyScreenContent(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/version")
	pressEnter(t, m)

	if m.activeScreen == nil {
		t.Fatalf("/version should open a screen overlay")
	}
	view := m.activeScreen.View()
	if !strings.Contains(view, "/version") {
		t.Errorf("BodyScreen view should contain command stripe '/version'; got:\n%s", view)
	}
	if !strings.Contains(view, "Esc") {
		t.Errorf("BodyScreen view should contain 'Esc to close' hint; got:\n%s", view)
	}
}

// TestContextRendersInline — /context renders inline as a chat-style
// info message, not as a modal overlay. Lock the claude-code-parity
// behavior added 2026-05-11.
func TestContextRendersInline(t *testing.T) {
	m := newSlashTestModel(t)
	before := len(m.messages)
	m.input.SetValue("/context")
	pressEnter(t, m)

	if m.activeScreen != nil {
		t.Errorf("/context should NOT open a screen overlay; got activeScreen=%T", m.activeScreen)
	}
	if len(m.messages) <= before {
		t.Errorf("/context should append at least one inline message; before=%d after=%d", before, len(m.messages))
	}
	// Spot-check the rendered content carries the headline.
	last := m.messages[len(m.messages)-1]
	if last.Role != "info" {
		t.Errorf("/context output should be an info-role message; got role=%q", last.Role)
	}
	if !strings.Contains(last.Content, "Context Usage") {
		t.Errorf("/context output should contain 'Context Usage' header; got: %q", last.Content)
	}
}

// TestInfoCommandsRenderInline — short status/info commands (/cost, /tokens)
// render inline in the transcript rather than opening a modal overlay that
// hides the conversation (claude-code parity, 2026-06-24 image #4).
func TestInfoCommandsRenderInline(t *testing.T) {
	for _, name := range []string{"cost", "tokens"} {
		t.Run("/"+name, func(t *testing.T) {
			m := newSlashTestModel(t)
			before := len(m.messages)
			m.input.SetValue("/" + name)
			pressEnter(t, m)

			if m.activeScreen != nil {
				t.Errorf("/%s should NOT open a screen overlay; got activeScreen=%T", name, m.activeScreen)
			}
			if len(m.messages) <= before {
				t.Errorf("/%s should append at least one inline message; before=%d after=%d", name, before, len(m.messages))
			}
		})
	}
}
