package tui

import (
	"errors"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/slash"
)

func TestTurnActive_ExportRunsLocallyOnce(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := newSlashTestModel(t)
	m.loop.AppendUser("conversation to export")
	m.turnActive = true
	before := len(m.messages)
	m.input.SetValue("/export")

	pressEnter(t, m)

	if len(m.queuedPrompts) != 0 {
		t.Fatalf("mid-turn /export must not enter the prompt queue: %+v", m.queuedPrompts)
	}
	if got := m.loop.SteerInjectDrainForTest(); got != "" {
		t.Fatalf("mid-turn /export must not be sent to the model: %q", got)
	}
	if len(m.messages) != before+2 {
		t.Fatalf("/export appended %d rows, want invocation + result", len(m.messages)-before)
	}
	invocation := m.messages[len(m.messages)-2]
	if invocation.Role != "user" || invocation.Content != "/export" {
		t.Fatalf("export invocation = %+v", invocation)
	}
	result := m.messages[len(m.messages)-1]
	if result.Role != "command-result" || !strings.HasPrefix(result.Content, "Conversation exported to: ") {
		t.Fatalf("export result = %+v", result)
	}
	path := strings.TrimPrefix(result.Content, "Conversation exported to: ")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("exported transcript %q: %v", path, err)
	}

	// A held/repeated Enter arrives after handleSubmit reset the editor. It
	// must not create another export row or queue a synthetic empty command.
	pressEnter(t, m)
	if len(m.messages) != before+2 || len(m.queuedPrompts) != 0 {
		t.Fatalf("repeated Enter duplicated export: messages=%d queue=%+v", len(m.messages)-before, m.queuedPrompts)
	}
}

func TestTurnActive_ClosingExportDoesNotQueue(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := newSlashTestModel(t)
	m.loop.AppendUser("finish now")
	out := make(chan agent.Event, 64)
	if err := m.loop.Run(m.ctx, out); err != nil {
		t.Fatalf("closing test loop: %v", err)
	}
	close(out)

	// Reproduce the screenshot's stale frame: loop steering is closed but the
	// TUI has not consumed doneCh and still reports an active turn.
	m.turnActive = true
	for _, r := range "/expo" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if !m.showPalette || len(m.palMatched) == 0 || m.palMatched[0].Name != "export" {
		t.Fatalf("precondition: /expo should select export, matches=%+v", m.palMatched)
	}
	before := len(m.messages)

	pressEnter(t, m)

	if len(m.queuedPrompts) != 0 {
		t.Fatalf("closing-turn /expo selection must not queue: %+v", m.queuedPrompts)
	}
	if got := m.loop.SteerInjectDrainForTest(); got != "" {
		t.Fatalf("closing-turn export must not reach steering: %q", got)
	}
	if len(m.messages) != before+2 || m.messages[len(m.messages)-2].Content != "/export" {
		t.Fatalf("palette export did not execute exactly once: %+v", m.messages[before:])
	}
	if !strings.HasPrefix(m.messages[len(m.messages)-1].Content, "Conversation exported to: ") {
		t.Fatalf("missing export success: %+v", m.messages[len(m.messages)-1])
	}
}

func TestTurnActive_ThinkingDisplayRunsLocally(t *testing.T) {
	m := newSlashTestModel(t)
	m.turnActive = true
	m.thinkingText = "line 1\nline 2\nline 3\nline 4\nline 5"
	m.input.SetValue("/thinking show")

	pressEnter(t, m)

	if m.thinkingDisplay != "show" {
		t.Fatalf("thinkingDisplay = %q, want show", m.thinkingDisplay)
	}
	if got := m.loop.SteerInjectDrainForTest(); got != "" {
		t.Fatalf("/thinking must not reach the model: %q", got)
	}
	if len(m.queuedPrompts) != 0 || m.input.Value() != "" {
		t.Fatalf("local command queued or retained input: queue=%+v input=%q", m.queuedPrompts, m.input.Value())
	}
	view := m.View().Content
	if !strings.Contains(view, "line 1") || !strings.Contains(view, "line 5") {
		t.Fatalf("show mode did not expand live reasoning: %s", view)
	}
}

func TestFinalizeTurn_ErrorStillRetainsOrdinaryQueuedPrompt(t *testing.T) {
	m := newSlashTestModel(t)
	m.turnActive = true
	m.enqueueQueuedItem("retry this work when connectivity returns", QueuePriorityNext)

	m.finalizeTurn(errors.New("provider EOF"))

	if len(m.queuedPrompts) != 1 || m.queuedPrompts[0].Text != "retry this work when connectivity returns" {
		t.Fatalf("error completion must preserve ordinary queued work: %+v", m.queuedPrompts)
	}
	if m.queuePending {
		t.Fatal("error completion must not auto-submit an ordinary queued prompt")
	}
}

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

// TestTurnActive_SubmitSteersCurrentTurn — pressing Enter while a turn
// is in flight injects ordinary text into that turn instead of silently
// deferring it as a separate queued turn.
func TestTurnActive_SubmitSteersCurrentTurn(t *testing.T) {
	m := newSlashTestModel(t)
	priorMsgCount := len(m.messages)
	for _, r := range "use Edit not Write" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m.turnActive = true

	pressEnter(t, m)

	if len(m.queuedPrompts) != 0 {
		t.Fatalf("plain mid-turn input must not queue; got %v", m.queuedPrompts)
	}
	if m.input.Value() != "" {
		t.Errorf("input should be cleared after steering; got %q", m.input.Value())
	}
	if got := m.loop.SteerInjectDrainForTest(); got != "use Edit not Write" {
		t.Errorf("steer buffer = %q, want submitted text", got)
	}
	if got := m.messages[priorMsgCount:]; len(got) != 1 || got[0].Role != "user-steer" || got[0].Content != "use Edit not Write" {
		t.Errorf("submitted steer should be visible as user-steer; got %+v", got)
	}
}

// TestTurnActive_LaterStillQueuesNextTurn — users retain an explicit
// way to defer work instead of steering the active turn.
func TestTurnActive_LaterStillQueuesNextTurn(t *testing.T) {
	m := newSlashTestModel(t)
	m.turnActive = true
	for _, r := range "/later verify after the current turn" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	pressEnter(t, m)

	if len(m.queuedPrompts) != 1 || m.queuedPrompts[0].Text != "verify after the current turn" {
		t.Fatalf("/later should queue its body for the next turn; got %+v", m.queuedPrompts)
	}
	if got := m.loop.SteerInjectDrainForTest(); got != "" {
		t.Errorf("/later must not steer the active turn; got %q", got)
	}
	if m.input.Value() != "" {
		t.Errorf("input should be cleared after /later; got %q", m.input.Value())
	}
}

// TestTurnActive_ClosingRaceFallsBackToQueue covers the render-tick window
// where Model.turnActive is still true but Loop.Run has already atomically
// closed steering. The input must land exactly once in the next-turn queue.
func TestTurnActive_ClosingRaceFallsBackToQueue(t *testing.T) {
	m := newSlashTestModel(t)
	m.loop.AppendUser("finish now")
	out := make(chan agent.Event, 64)
	if err := m.loop.Run(m.ctx, out); err != nil {
		t.Fatalf("closing test loop: %v", err)
	}
	close(out)

	m.turnActive = true // simulate one stale TUI frame before LoopDone update
	for _, r := range "one last instruction" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	pressEnter(t, m)

	if len(m.queuedPrompts) != 1 || m.queuedPrompts[0].Text != "one last instruction" {
		t.Fatalf("closing-race input should queue exactly once; got %+v", m.queuedPrompts)
	}
	if got := m.loop.SteerInjectDrainForTest(); got != "" {
		t.Fatalf("closing-race input must not remain in steer buffer: %q", got)
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != "info" || !strings.Contains(last.Content, "queued for the next turn") {
		t.Fatalf("closing-race fallback should be visible; got %+v", last)
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

// TestTurnActive_SafeSlashFallsThroughToSteer — known informational
// slash commands do not start another dispatcher while a turn is active;
// their literal text is made available to the running model instead.
func TestTurnActive_SafeSlashFallsThroughToSteer(t *testing.T) {
	m := newSlashTestModel(t)
	m.turnActive = true
	for _, r := range "/cost" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	pressEnter(t, m)

	if got := m.loop.SteerInjectDrainForTest(); got != "/cost" {
		t.Errorf("safe slash should steer literal input; got %q", got)
	}
	if len(m.queuedPrompts) != 0 {
		t.Errorf("safe slash must not queue; got %+v", m.queuedPrompts)
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != "user-steer" || last.Content != "/cost" {
		t.Errorf("safe slash steer should be visible; got %+v", last)
	}
}

// TestTurnActive_UnknownSlashFallsThroughToSteer — unknown /<command>
// goes through the steer path as literal text. This is the safe
// default — user might be typing actual chat content that happens to
// start with a slash.
func TestTurnActive_UnknownSlashFallsThroughToSteer(t *testing.T) {
	m := newSlashTestModel(t)
	m.turnActive = true
	for _, r := range "/notarealcommand" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	pressEnter(t, m)

	if got := m.loop.SteerInjectDrainForTest(); got != "/notarealcommand" {
		t.Errorf("unknown slash should steer literal input; got %q", got)
	}
	if len(m.queuedPrompts) != 0 {
		t.Errorf("unknown slash must not queue; got %v", m.queuedPrompts)
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != "user-steer" || last.Content != "/notarealcommand" {
		t.Errorf("unknown slash steer should be visible; got %+v", last)
	}
}
