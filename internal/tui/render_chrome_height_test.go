package tui

// Regression tests for the two interaction bugs reported 2026-05-07
// (image #2 / image #3): the bottom of the chat surface gets clipped
// when permission prompt or other lower-chrome rendering pushes the
// total output past terminal height, and bracketed-paste content was
// silently dropped because Update() had no tea.PasteMsg case.
//
// Both bugs traced to tui_render.go's hardcoded `m.height - 10` chrome
// reserve and a missing handler in tui_update.go. These tests freeze
// the post-fix behavior:
//
//   - Permission prompt with 4 choices remains fully visible at
//     terminal height 30 (was clipping options 3-4 before).
//   - PasteMsg content lands in the textarea via InsertString.

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

// TestPermissionPrompt_AllChoicesVisible — when permActive is on, the
// View() output must include the full 4-choice list. Regression: the
// previous tui_render.go reserved a fixed 10 chrome rows; permission
// prompt actually occupies ~11 rows, so options 3-4 fell off the
// visible window when the chat list filled the rest.
func TestPermissionPrompt_AllChoicesVisible(t *testing.T) {
	m := newSlashTestModel(t)
	m.height = 30 // typical terminal height where the bug surfaces
	m.width = 100

	m.permActive = true
	m.permTool = "Bash"
	m.permArgs = "find . -name 'metis*'"
	m.permQuestion = "Allow Bash command?"
	m.permChoices = []permChoice{
		{Label: "Yes", Key: "y"},
		{Label: "Yes, always", Key: "a"},
		{Label: "No", Key: "n"},
		{Label: "Cancel turn", Key: "c"},
	}
	m.permCursor = 0
	m.permReply = make(chan agent.PermissionDecision, 1)
	m.permStartedAt = time.Now()

	// Pad the chat list with enough messages that the OLD behavior
	// (reserve only 10 chrome rows) would push permission off screen.
	for i := 0; i < 25; i++ {
		m.messages = append(m.messages, Message{
			Role:      "user",
			Content:   "filler line " + strings.Repeat("x", 60),
			Timestamp: time.Now(),
		})
	}

	view := m.View()
	// Strip ANSI: each choice line gets multi-segment SGR coloring
	// (cursor / number / label can land in different runs), so a raw
	// strings.Contains misses "2. Yes, always" even when the bytes
	// are present.
	out := ansi.Strip(view.Content)

	for _, want := range []string{"1. Yes", "2. Yes, always", "3. No", "4. Cancel turn"} {
		if !strings.Contains(out, want) {
			t.Errorf("permission choice %q missing from rendered View; full output:\n%s", want, out)
		}
	}
}

// TestPaste_TextLandsInInput — bracketed paste must reach the
// textarea. Regression: tui_update.go switch had no PasteMsg case so
// pasted content was silently dropped, leaving the cmd+V experience
// broken even though the explicit Ctrl+V path (system clipboard read)
// worked.
func TestPaste_TextLandsInInput(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("")

	pasted := "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"hi\") }"
	m.Update(tea.PasteMsg{Content: pasted})

	if got := m.input.Value(); got != pasted {
		t.Errorf("paste content not in input; got %q want %q", got, pasted)
	}
}

// TestPaste_BlockedDuringPermission — pasting must be a no-op while a
// permission prompt is active so the user doesn't accidentally queue
// content that flashes through to the next turn.
func TestPaste_BlockedDuringPermission(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("orig")
	m.permActive = true
	m.permChoices = []permChoice{{Label: "Yes", Key: "y"}}

	m.Update(tea.PasteMsg{Content: "should not arrive"})

	if got := m.input.Value(); got != "orig" {
		t.Errorf("paste leaked through permission prompt; input=%q", got)
	}
}

// TestPaste_TruncatedAtCap — a giant paste (>100KB) gets truncated
// with a visible info row so the user knows what happened.
func TestPaste_TruncatedAtCap(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("")

	huge := strings.Repeat("a", 200*1024) // 200 KB
	m.Update(tea.PasteMsg{Content: huge})

	got := m.input.Value()
	if len(got) > 100*1024 {
		t.Errorf("paste should be capped at 100KB; got %d bytes", len(got))
	}
	// Should have appended an info row mentioning truncation.
	found := false
	for _, msg := range m.messages {
		if msg.Role == "info" && strings.Contains(msg.Content, "paste truncated") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("missing 'paste truncated' info row; messages=%+v", messageContents(m))
	}
}
