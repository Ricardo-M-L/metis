package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestBtwOverlay_EscThenEnterDoesNotResend verifies the bug where pressing
// Esc to dismiss a /btw overlay followed by Enter would re-submit the
// original "/btw …" text as a regular user message. startBtwQuery now
// resets the input after pushing the overlay, so the next Enter sees an
// empty input box and handleSubmit returns early.
func TestBtwOverlay_E2E_EscThenEnterDoesNotResend(t *testing.T) {
	mockAnswer := "answer"
	mockAsk := func(_ context.Context, q string) (string, error) {
		return mockAnswer, nil
	}
	m := newBtwTestModel(t, mockAsk)

	// Step 1: user types "/btw hello" and submits
	m.input.SetValue("/btw hello")
	pushCmd := pressEnter(t, m)

	// After handleSubmit, the overlay should be on the stack.
	if top := m.overlays.Top(); top == nil || top.Name() != "btw" {
		t.Fatalf("expected btw overlay on stack after Enter")
	}

	// After handleSubmit, the input textarea must be empty — this is the
	// invariant that prevents the Esc+Enter re-send bug. startBtwQuery
	// enforces this.
	if v := m.input.Value(); v != "" {
		t.Errorf("after /btw submit, input should be empty, got %q", v)
	}

	// Step 2: deliver the answer (simulating OnPush cmd completion)
	answerMsg := runCmd(t, pushCmd)
	if answerMsg == nil {
		t.Fatalf("OnPush Cmd should produce a BtwAnswerMsg")
	}
	updatedModel, _ := m.Update(answerMsg)
	m = updatedModel.(*Model)

	// Step 3: user hits Esc to dismiss the overlay
	updatedModel, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updatedModel.(*Model)

	if m.overlays.Active() {
		t.Errorf("overlay should be dismissed after Esc")
	}

	// Step 4: user hits Enter again — this should be a NO-OP because the
	// input is empty. Previously this would re-submit "/btw hello" as a
	// user message.
	beforeLen := len(m.messages)
	updatedModel, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updatedModel.(*Model)

	if len(m.messages) > beforeLen {
		t.Errorf("Esc+Enter should not append a new message (before=%d, after=%d)",
			beforeLen, len(m.messages))
	}
	if v := m.input.Value(); v != "" {
		t.Errorf("input should still be empty after no-op Enter, got %q", v)
	}
}