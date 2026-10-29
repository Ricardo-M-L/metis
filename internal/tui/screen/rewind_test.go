package screen

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func rewindEntries() []RewindEntry {
	return []RewindEntry{
		{Turn: 3, Prompt: "third prompt", HasCodeCheckpoint: false},
		{Turn: 2, Prompt: "second prompt", HasCodeCheckpoint: true, LatestEdit: true},
		{Turn: 1, Prompt: "first prompt", HasCodeCheckpoint: true},
	}
}

func TestRewindScreen_SelectPointThenAction(t *testing.T) {
	s := NewRewindScreen(rewindEntries())
	s.Resize(100, 30)
	if view := s.View(); !strings.Contains(view, "third prompt") || !strings.Contains(view, "Select a checkpoint") {
		t.Fatalf("point view missing prompt/header:\n%s", view)
	}

	s.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if s.Done() {
		t.Fatal("point Enter should advance to action stage, not close")
	}
	if view := s.View(); !strings.Contains(view, "Restore code and conversation") || !strings.Contains(view, "Summarize from here") {
		t.Fatalf("action view missing Claude-style choices:\n%s", view)
	}
	s.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !s.Done() || s.SelectedTurn() != 2 || s.Action() != RewindActionConversation {
		t.Fatalf("result: done=%v turn=%d action=%v", s.Done(), s.SelectedTurn(), s.Action())
	}
}

func TestRewindScreen_EscapeReturnsFromActionsThenCancels(t *testing.T) {
	s := NewRewindScreen(rewindEntries())
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	s.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if s.Done() {
		t.Fatal("Esc from actions should return to checkpoint list")
	}
	s.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !s.Done() || s.Action() != RewindActionCancel {
		t.Fatalf("second Esc should cancel: done=%v action=%v", s.Done(), s.Action())
	}
}

func TestRewindScreen_QuickLatestEditKeepsLegacyBehavior(t *testing.T) {
	s := NewRewindScreen(rewindEntries())
	s.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	if !s.Done() || s.SelectedTurn() != 2 || s.Action() != RewindActionBoth {
		t.Fatalf("quick last edit: done=%v turn=%d action=%v", s.Done(), s.SelectedTurn(), s.Action())
	}
}

func TestRewindScreen_ConversationPointDisablesCodeActions(t *testing.T) {
	s := NewRewindScreen(rewindEntries())
	s.Resize(100, 30)
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if s.actionCursor != 1 {
		t.Fatalf("point without a code snapshot selected action %d, want conversation", s.actionCursor)
	}
	if view := s.View(); !strings.Contains(view, "unavailable") {
		t.Fatalf("missing explanation for unavailable code restore:\n%s", view)
	}
	s.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if s.actionCursor != 3 {
		t.Fatalf("navigation selected disabled code action: %d", s.actionCursor)
	}
	s.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if s.actionCursor != 1 {
		t.Fatalf("backward navigation selected disabled code action: %d", s.actionCursor)
	}
	s.actionCursor = 0
	s.selectCurrentAction()
	if s.Done() {
		t.Fatal("disabled combined restore was accepted")
	}
	s.actionCursor = 2
	s.selectCurrentAction()
	if s.Done() {
		t.Fatal("disabled code restore was accepted")
	}
}

func TestRewindScreen_QuickLatestEditRejectsUnavailableCode(t *testing.T) {
	s := NewRewindScreen([]RewindEntry{{Turn: 1, Prompt: "uncaptured shell edit", LatestEdit: true}})
	s.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	if s.Done() {
		t.Fatal("quick restore accepted an incomplete code checkpoint")
	}
}

func TestRewindScreen_UnavailableReasonIsVisibleAndSanitized(t *testing.T) {
	s := NewRewindScreen([]RewindEntry{{Turn: 1, Prompt: "shell turn", CodeUnavailableReason: "Bash write scope is unknown\x1b[31m"}})
	s.Resize(100, 30)
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	view := s.View()
	if !strings.Contains(view, "Bash write scope is unknown") || strings.Contains(view, "\x1b[31m") {
		t.Fatalf("unavailable reason not safely displayed: %q", view)
	}
}

func TestRewindPromptPreviewNeutralizesTerminalControls(t *testing.T) {
	original := "你好 \x1b]52;c;YXR0YWNr\a clipboard \x1b[31mred\x1b[0m \x9dtitle\x9c 世界"
	preview := rewindPromptPreview(original, 200)
	for _, control := range []string{"\x1b", "\a", "\x9d", "\x9c"} {
		if strings.Contains(preview, control) {
			t.Fatalf("preview retained terminal control %q: %q", control, preview)
		}
	}
	if !strings.Contains(preview, "你好") || !strings.Contains(preview, "世界") {
		t.Fatalf("preview lost ordinary Unicode: %q", preview)
	}
	// Sanitizing the rendered preview must never modify the original prompt
	// which is returned verbatim to the editor after selection.
	entry := RewindEntry{Turn: 1, Prompt: original}
	s := NewRewindScreen([]RewindEntry{entry})
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if s.entries[0].Prompt != original {
		t.Fatalf("screen mutated composer prompt: %q", s.entries[0].Prompt)
	}
}
