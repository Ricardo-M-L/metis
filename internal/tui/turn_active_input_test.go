package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/slash"
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
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	got := m.input.Value()
	if got != "hello" {
		t.Errorf("turnActive should NOT eat keystrokes; input.Value() = %q, want %q", got, "hello")
	}
}

// TestTurnActive_SubmitQueues — post-task#35 behavior (claude-code
// parity): pressing Enter while a turn is in flight queues the input
// onto m.queuedPrompts instead of steer-injecting into the running
// turn. The queue drains on turn end (finalizeTurn) by loading the
// head into the input and firing handleSubmit on the next tick.
//
// Steering remains available via slash mid-turn handling for users
// who explicitly want it — only plain text routes through the queue.
func TestTurnActive_SubmitQueues(t *testing.T) {
	m := newSlashTestModel(t)
	for _, r := range "use Edit not Write" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m.turnActive = true

	pressEnter(t, m)

	// Verify the queue holds the typed text.
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("expected 1 queued prompt; got %d (%v)", len(m.queuedPrompts), m.queuedPrompts)
	}
	if m.queuedPrompts[0].Text != "use Edit not Write" {
		t.Errorf("queue head wrong; got %q", m.queuedPrompts[0].Text)
	}
	// Input must be cleared so the user sees the queue acceptance
	// (matches normal-submit affordance — input clears on send).
	if m.input.Value() != "" {
		t.Errorf("input should be cleared after queueing; got %q", m.input.Value())
	}
	// The agent loop's steer buffer must NOT have been touched —
	// queueing is a different pipeline.
	if got := m.loop.SteerInjectDrainForTest(); got != "" {
		t.Errorf("steer buffer should be empty; got %q", got)
	}
}

// TestTurnActive_AcceptsBackspace — typos must be fixable mid-turn.
// Pre-fix: backspace was eaten by the key gate. Post-fix: it edits
// the textarea normally.
func TestTurnActive_AcceptsBackspace(t *testing.T) {
	m := newSlashTestModel(t)
	for _, r := range "helloo" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m.turnActive = true

	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})

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
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})

	if m.chatList.AtBottom() {
		t.Errorf("MouseWheelUp during turnActive should leave AtBottom() = false; still at bottom")
	}
}

// TestTurnActive_DestructiveSlashRefused — Task #87. /clear typed
// mid-turn must refuse with the "press Esc to cancel" hint. Without
// this guard the literal text "/clear" would just go into steerBuf,
// confusing the user who clearly wanted to actually clear.
func TestTurnActive_DestructiveSlashRefused(t *testing.T) {
	m := newSlashTestModel(t)
	// Set turnActive BEFORE typing /clear — otherwise the live-fire
	// shortcut at keybind_main.go (which now respects turnActive)
	// would still fire during typing in the test, since the test
	// hasn't yet set the flag. Real-world usage: user starts a turn
	// (turnActive=true), then types /clear; the typing must not
	// trigger the shortcut.
	m.turnActive = true
	for _, r := range "/clear" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	beforeMsgs := len(m.messages)

	pressEnter(t, m)

	// Look for the refusal hint.
	found := false
	for i := beforeMsgs; i < len(m.messages); i++ {
		if strings.Contains(m.messages[i].Content, "can't /clear mid-turn") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("destructive /clear mid-turn must surface refusal hint; messages[%d:]=%+v", beforeMsgs, m.messages[beforeMsgs:])
	}
	// Must NOT have been steered.
	if got := m.loop.SteerInjectDrainForTest(); got != "" {
		t.Errorf("destructive slash must NOT reach steerBuf; got %q", got)
	}
}

// TestTurnActive_CustomSlashResolvesAndSteers — Task #87. A custom
// command typed mid-turn should resolve its template and SteerInject
// the resolved TEXT, not the literal "/intro Chinese" string.
func TestTurnActive_CustomSlashResolvesAndSteers(t *testing.T) {
	m := newSlashTestModel(t)
	// Inject a custom command into the registry directly (mimics what
	// LoadCustomCommands does at startup).
	m.slash.Register(slash.Cmd{
		Name:        "intro",
		Description: "test custom",
		Handler: func(args string) (string, slash.Signal) {
			return "Briefly introduce yourself in " + args + " in one sentence.", slash.SignalCustomPrompt
		},
	})
	m.turnActive = true
	for _, r := range "/intro Chinese" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	pressEnter(t, m)

	// SteerBuf should hold the RESOLVED template, not the literal "/intro Chinese".
	steered := m.loop.SteerInjectDrainForTest()
	if !strings.Contains(steered, "Briefly introduce yourself in Chinese") {
		t.Errorf("custom slash should steer resolved template; got %q", steered)
	}
	if strings.Contains(steered, "/intro") {
		t.Errorf("steered text must be RESOLVED template, not literal slash; got %q", steered)
	}
}

// TestTurnActive_UnknownSlashFallsThroughToSteer — unknown /<command>
// goes through the steer path as literal text. This is the safe
// default — user might be typing actual chat content that happens to
// start with a slash.
func TestTurnActive_UnknownSlashFallsThroughToQueue(t *testing.T) {
	// Post-task#35: unknown slash (MidTurnSafe → fall-through) lands
	// in the queue, not the steer buffer. The slash itself isn't a
	// known command so we treat the literal text as a future user
	// turn, matching claude-code's "next message" semantics.
	m := newSlashTestModel(t)
	m.turnActive = true
	for _, r := range "/notarealcommand" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	pressEnter(t, m)

	if got := m.loop.SteerInjectDrainForTest(); got != "" {
		t.Errorf("steer buffer should be empty after queueing; got %q", got)
	}
	if len(m.queuedPrompts) != 1 || m.queuedPrompts[0].Text != "/notarealcommand" {
		t.Errorf("expected single queued prompt with literal slash; got %v", m.queuedPrompts)
	}
}
