package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	"github.com/Ricardo-M-L/metis/internal/tui/list"
	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tui/overlay"
)

// TestBtwOverlay_E2E_FullFlow simulates the entire /btw lifecycle the way
// it would happen in a real chat session, without spinning up a terminal
// or hitting an LLM API:
//
//  1. user types "/btw 你好" and submits
//  2. slash registry routes SignalBtw
//  3. keybind_submit calls startBtwQuery
//  4. overlay.Push fires OnPush — returns a Cmd that "asks" the LLM
//  5. View() shows loading state ("thinking...")
//  6. mock LLM returns answer → BtwAnswerMsg flows back to Update
//  7. View() shows answer
//  8. user hits Esc → overlay dismisses
//  9. View() no longer contains the modal
//
// We swap out m.ext.BtwAsk for a deterministic mock so the test never
// touches the network. Same for the slash registry — we use the real
// one so we exercise the actual SignalBtw → startBtwQuery wiring.
func TestBtwOverlay_E2E_FullFlow(t *testing.T) {
	mockAnswer := "the answer is 42"
	asked := make(chan string, 1)
	mockAsk := func(_ context.Context, q string) (string, error) {
		asked <- q
		return mockAnswer, nil
	}

	m := newBtwTestModel(t, mockAsk)

	// --- step 1+2+3: simulate user typing "/btw 你好" + Enter ------------------
	// Set the textarea content directly — bypassing keystroke-by-keystroke
	// typing keeps the test fast + readable.
	m.input.SetValue("/btw 你好")

	// Fire Enter through the same handleKey path real keystrokes use.
	// The returned Cmd is the OnPush Cmd from BtwOverlay — it's what
	// invokes mockAsk and produces the BtwAnswerMsg.
	pushCmd := pressEnter(t, m)

	// --- step 4: overlay should now exist + be top + active ------------------
	top := m.overlays.Top()
	if top == nil {
		t.Fatalf("expected /btw overlay on stack, got nil top")
	}
	if top.Name() != "btw" {
		t.Fatalf("top overlay name = %q, want %q", top.Name(), "btw")
	}
	if !top.Active() {
		t.Fatalf("btw overlay should be active immediately after push")
	}

	// --- step 5: View should show the loading state --------------------------
	view := m.View().Content
	if !strings.Contains(view, "/btw") {
		t.Errorf("View missing /btw title:\n%s", view)
	}
	if !strings.Contains(view, "你好") {
		t.Errorf("View missing user question:\n%s", view)
	}
	if !strings.Contains(view, "thinking") {
		t.Errorf("View missing loading indicator:\n%s", view)
	}
	if strings.Contains(view, mockAnswer) {
		t.Errorf("View should NOT contain the answer yet (still loading)")
	}

	// --- step 6: run the OnPush Cmd to invoke mockAsk + deliver answer -------
	// In production bubbletea executes pushCmd on a goroutine; here we
	// run it synchronously and feed the resulting Msg back to Update,
	// which is exactly what the runtime would do.
	answerMsg := runCmd(t, pushCmd)
	if answerMsg == nil {
		t.Fatalf("OnPush Cmd should produce a BtwAnswerMsg, got nil")
	}
	select {
	case got := <-asked:
		if got != "你好" {
			t.Errorf("mock backend received %q, want %q", got, "你好")
		}
	case <-time.After(time.Second):
		t.Fatalf("mock LLM was never called")
	}

	updatedModel, _ := m.Update(answerMsg)
	m = updatedModel.(*Model)

	// --- step 7: View should now show the answer ----------------------------
	view = m.View().Content
	if !strings.Contains(view, mockAnswer) {
		t.Errorf("after answer delivery, View missing answer text:\n%s", view)
	}
	if strings.Contains(view, "thinking") {
		t.Errorf("after answer delivery, loading indicator should be gone:\n%s", view)
	}

	// --- step 8: Esc dismisses ----------------------------------------------
	updatedModel, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updatedModel.(*Model)

	if m.overlays.Active() {
		t.Errorf("after Esc, overlay stack should have no active overlay")
	}

	// --- step 9: View should no longer render the modal ----------------------
	view = m.View().Content
	if strings.Contains(view, "/btw — side question") {
		t.Errorf("after Esc, modal title should be gone:\n%s", view)
	}
	if strings.Contains(view, mockAnswer) {
		t.Errorf("after Esc, answer should not be visible:\n%s", view)
	}
}

func TestBtwOverlay_E2E_ErrorPath(t *testing.T) {
	mockAsk := func(_ context.Context, _ string) (string, error) {
		return "", errors.New("api_error: rate limited")
	}
	m := newBtwTestModel(t, mockAsk)

	m.input.SetValue("/btw whatever")
	pressEnter(t, m)

	if m.overlays.Top() == nil {
		t.Fatalf("overlay should have been pushed even before error arrives")
	}

	updatedModel, _ := m.Update(overlay.BtwAnswerMsg{Err: errors.New("api_error: rate limited")})
	m = updatedModel.(*Model)

	view := m.View().Content
	if !strings.Contains(view, "error") {
		t.Errorf("View should mention 'error':\n%s", view)
	}
	if !strings.Contains(view, "rate limited") {
		t.Errorf("View should surface error reason:\n%s", view)
	}
}

func TestBtwOverlay_E2E_NoBackendShowsImmediateError(t *testing.T) {
	// Construct without a mockAsk — exercise the "/btw not wired" path.
	m := newBtwTestModel(t, nil)

	m.input.SetValue("/btw orphan")
	pressEnter(t, m)

	// The overlay still pushes (so the user sees what went wrong),
	// but goes straight to the error state.
	top := m.overlays.Top()
	if top == nil {
		t.Fatalf("overlay should push even when no backend is wired")
	}

	view := m.View().Content
	if !strings.Contains(view, "not wired") {
		t.Errorf("View should explain absent backend:\n%s", view)
	}
}

func TestBtwOverlay_E2E_DuplicateInvocationReplaces(t *testing.T) {
	// Two consecutive /btw invocations should leave exactly ONE overlay
	// on the stack — the second replaces the first (claude-code parity).
	mockAsk := func(_ context.Context, q string) (string, error) {
		return "answer to: " + q, nil
	}
	m := newBtwTestModel(t, mockAsk)

	m.input.SetValue("/btw first question")
	pressEnter(t, m)
	if m.overlays.Len() != 1 {
		t.Fatalf("first /btw: overlays.Len = %d, want 1", m.overlays.Len())
	}

	m.input.SetValue("/btw second question")
	pressEnter(t, m)
	if m.overlays.Len() != 1 {
		t.Fatalf("after second /btw: overlays.Len = %d, want 1 (replace, not append)", m.overlays.Len())
	}

	// The overlay should reflect the SECOND question, not the first.
	view := m.View().Content
	if !strings.Contains(view, "second question") {
		t.Errorf("View should show second question:\n%s", view)
	}
	if strings.Contains(view, "first question") {
		t.Errorf("View should NOT show first question after replace")
	}
}

// ============================================================================
// helpers
// ============================================================================

// newBtwTestModel constructs a Model wired enough to exercise /btw without
// any external dependencies. We deliberately set width/height so View()
// renders deterministically (BtwOverlay uses width to size its box).
func newBtwTestModel(t *testing.T, ask func(context.Context, string) (string, error)) *Model {
	t.Helper()

	ti := textarea.New()
	ti.SetWidth(80)
	ti.Focus()
	cl := list.NewList()
	cl.SetSize(78, 20)

	// Real slash registry — we want the actual SignalBtw routing.
	slashReg := slash.NewRegistry()
	slash.RegisterAll(slashReg, nil)

	m := &Model{
		ctx:       context.Background(),
		gate:      permission.New(permission.ModeAuto),
		slash:     slashReg,
		cmds:      BuildREPLCommands(),
		startTime: time.Now(),
		input:     ti,
		chatList:  cl,
		overlays:  overlay.New(),
		width:     90,
		height:    30,
		// eventCh / loop deliberately left nil — /btw doesn't go through
		// the agent loop, so these are never read on the path we exercise.
		// firstRender=false so View() takes the "transcript present" branch
		// instead of the welcome-banner-only branch (which doesn't render
		// overlays).
		firstRender: false,
		showBanner:  false,
	}
	// Seed at least one message so the View() takes the non-welcome path
	// (which is where overlays render). Empty messages = welcome banner.
	m.messages = append(m.messages, Message{Role: "info", Content: "(test session)", Timestamp: time.Now()})

	if ask != nil {
		m.SetExternalHooks(ExternalHooks{BtwAsk: ask})
	}
	return m
}

// pressEnter sends KeyEnter through Update and returns the resulting Cmd.
// In real bubbletea the runtime executes Cmd in a goroutine; tests must
// invoke it manually to drive async work like the OnPush LLM call.
func pressEnter(t *testing.T, m *Model) tea.Cmd {
	t.Helper()
	updatedModel, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := updatedModel.(*Model)
	*m = *got
	return cmd
}

// runCmd executes a tea.Cmd synchronously and returns the produced Msg.
// Mirrors what bubbletea's runtime does in a goroutine, but blocking so
// tests can assert on the result deterministically.
func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	return cmd()
}
