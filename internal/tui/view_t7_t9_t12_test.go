package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

// view_t7_t9_t12_test.go — view-level integration tests for the three
// remaining T-series features that previously had only unit-level
// coverage (handler logic + raw API). These add the missing piece:
// drive the full Update → handler → spinner/state path with realistic
// inputs so a regression in keybind dispatch or event routing fails
// loudly.
//
// Why view-level: the unit tests in keybind_history_direct_test.go,
// theme_provider_test.go, and tool_args_preview_test.go cover the
// state machines correctly, but they call the helpers directly. They
// don't catch a bug like "↑ key wasn't routed because the keybind
// switch returned early on a different case." Each test below feeds
// the integration boundary (KeyMsg / Event / cmdTheme) and reads back
// the user-visible side-effect.

// ---------- T7 ↑/↓ direct prompt history ----------

// TestT7_UpArrowOnEmptyInput_LoadsPreviousPrompt — pressing ↑ when
// the input is empty should kick off direct-history nav and load the
// most recent histAll entry into the textarea. This is the exact key
// path keybind_main.go intercepts (case "up" inside the eligible
// guard) — drives all the way through Update.
func TestT7_UpArrowOnEmptyInput_LoadsPreviousPrompt(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.histAll = []string{"latest prompt", "older prompt"}
	m.histDirectIdx = -1

	drive(t, m, tea.KeyPressMsg{Code: tea.KeyUp})

	if got := m.input.Value(); got != "latest prompt" {
		t.Errorf("↑ on empty input should load histAll[0]; got %q", got)
	}
	if m.histDirectIdx != 0 {
		t.Errorf("histDirectIdx should be 0 after first ↑; got %d", m.histDirectIdx)
	}
}

// TestT7_SecondUpArrow_StepsBackFurther — second ↑ press should walk
// to histAll[1] (older).
func TestT7_SecondUpArrow_StepsBackFurther(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.histAll = []string{"newer", "older"}
	m.histDirectIdx = -1

	drive(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	drive(t, m, tea.KeyPressMsg{Code: tea.KeyUp})

	if got := m.input.Value(); got != "older" {
		t.Errorf("two ↑ presses should land on histAll[1]; got %q", got)
	}
}

// TestT7_DownArrow_RestoresDraftAfterUp — ↑ saves the draft, ↓ past
// idx 0 restores it. Verifies the draft round-trip through the
// keybind dispatcher (not just the helper).
func TestT7_DownArrow_RestoresDraftAfterUp(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.histAll = []string{"old"}
	m.input.SetValue("draft so far")
	m.histDirectIdx = -1

	// Force eligibility: in our path, eligible iff input == "" OR
	// histDirectIdx >= 0. Non-empty draft + idx == -1 is not eligible
	// — that's intentional (lets textarea handle ↑↓ as cursor moves
	// when the user is editing). To reach the nav path we have to
	// start nav explicitly.
	m.directHistoryUp()           // saves "draft so far", loads "old"
	if m.input.Value() != "old" { // sanity
		t.Fatalf("setup: nav should have loaded 'old'; got %q", m.input.Value())
	}

	drive(t, m, tea.KeyPressMsg{Code: tea.KeyDown})

	if m.histDirectIdx != -1 {
		t.Errorf("↓ past idx 0 should exit nav (idx=-1); got %d", m.histDirectIdx)
	}
	if got := m.input.Value(); got != "draft so far" {
		t.Errorf("draft should be restored; got %q", got)
	}
}

// TestT7_PrintableKeyExitsNavMode — once in nav mode, typing a rune
// should drop the nav flag so the next ↑ starts fresh.
func TestT7_PrintableKeyExitsNavMode(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.histAll = []string{"prev"}
	m.histDirectIdx = -1

	// Enter nav mode.
	drive(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.histDirectIdx < 0 {
		t.Fatalf("setup: ↑ should enter nav mode; idx=%d", m.histDirectIdx)
	}

	// Type a rune — should reset nav.
	drive(t, m, tea.KeyPressMsg{Text: "x", Code: 'x'})

	if m.histDirectIdx != -1 {
		t.Errorf("typing a rune should exit nav (idx=-1); got %d", m.histDirectIdx)
	}
}

// ---------- T9 /theme auto retint ----------

// TestT9_ThemeAutoForOpenAI_RetintsAccent — calling cmdTheme with
// "auto:openai" should mutate currentTheme to a clone with the openai
// brand color in AccentBlue. Restored at test end so we don't leak
// state across tests.
func TestT9_ThemeAutoForOpenAI_RetintsAccent(t *testing.T) {
	originalName := currentTheme.Name
	originalAccent := currentTheme.AccentBlue
	t.Cleanup(func() {
		// Restore by re-running SwitchTheme on the original base
		// name (strips any "+provider" suffix).
		base := strings.SplitN(originalName, "+", 2)[0]
		SwitchTheme(base)
	})

	r := &REPL{}
	got := cmdTheme(r, "auto:openai")

	if !strings.Contains(got, "openai") {
		t.Errorf("cmdTheme(auto:openai) should report new theme name; got %q", got)
	}
	if !strings.Contains(currentTheme.Name, "openai") {
		t.Errorf("currentTheme.Name should reflect provider tint; got %q", currentTheme.Name)
	}
	if currentTheme.AccentBlue == originalAccent {
		t.Error("AccentBlue should change after auto:openai retint")
	}
}

// TestT9_ThemeAutoUnknownProvider_NoMutation — auto:notarealprovider
// should leave currentTheme unchanged (ApplyProviderTint returns
// early when the provider id is unknown).
func TestT9_ThemeAutoUnknownProvider_NoMutation(t *testing.T) {
	originalName := currentTheme.Name
	originalAccent := currentTheme.AccentBlue
	t.Cleanup(func() {
		base := strings.SplitN(originalName, "+", 2)[0]
		SwitchTheme(base)
	})

	r := &REPL{}
	cmdTheme(r, "auto:not-a-real-provider")

	if currentTheme.Name != originalName {
		t.Errorf("unknown provider should leave theme name alone; before=%q after=%q",
			originalName, currentTheme.Name)
	}
	if currentTheme.AccentBlue != originalAccent {
		t.Error("unknown provider should leave AccentBlue alone")
	}
}

// ---------- T12 streaming tool args spinner ----------

// TestT12_ToolArgsDeltaEvent_SetsSpinnerSubline — feeding an
// EventToolArgsDelta with partial JSON should cause m.spinnerSub to
// reflect the in-flight tool name + first value preview. Catches a
// regression where tui_events.go's EventToolArgsDelta case might be
// missed or the buffer routing might be wrong.
func TestT12_ToolArgsDeltaEvent_SetsSpinnerSubline(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)

	m.handleAgentEvent(agent.Event{
		Kind:      agent.EventToolArgsDelta,
		ToolName:  "Read",
		ToolUseID: "t1",
		TextDelta: `{"path":"/tmp/foo.go"}`,
	})

	if m.spinnerSub == "" {
		t.Fatalf("spinnerSub should be populated by EventToolArgsDelta; got empty")
	}
	if !strings.Contains(m.spinnerSub, "Read") {
		t.Errorf("spinnerSub should mention tool name; got %q", m.spinnerSub)
	}
	if !strings.Contains(m.spinnerSub, "/tmp/foo.go") {
		t.Errorf("spinnerSub should preview first value; got %q", m.spinnerSub)
	}
	if len(m.toolArgsStream) == 0 {
		t.Error("toolArgsStream buffer should accumulate the delta bytes")
	}
}

// TestT12_ToolArgsDeltaThenResult_ResetsBuffer — toolArgsStream
// should clear on EventToolResult so the next tool starts clean.
func TestT12_ToolArgsDeltaThenResult_ResetsBuffer(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)

	m.handleAgentEvent(agent.Event{
		Kind:      agent.EventToolArgsDelta,
		ToolName:  "Read",
		ToolUseID: "t1",
		TextDelta: `{"path":"/tmp"}`,
	})
	if len(m.toolArgsStream) == 0 {
		t.Fatal("setup: buffer should be populated")
	}

	m.handleAgentEvent(agent.Event{
		Kind:       agent.EventToolResult,
		ToolName:   "Read",
		ToolUseID:  "t1",
		ToolResult: &agent.ToolResult{Output: "foo content"},
	})

	if len(m.toolArgsStream) != 0 {
		t.Errorf("EventToolResult should reset toolArgsStream; got %d bytes",
			len(m.toolArgsStream))
	}
}

// TestT12_MultipleDeltas_AccumulateInBuffer — successive deltas for
// the same tool should append; spinnerSub re-shapes from the full
// buffer each time so the preview can grow.
func TestT12_MultipleDeltas_AccumulateInBuffer(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)

	m.handleAgentEvent(agent.Event{
		Kind: agent.EventToolArgsDelta, ToolName: "Bash", ToolUseID: "b1",
		TextDelta: `{"command":"git st`,
	})
	m.handleAgentEvent(agent.Event{
		Kind: agent.EventToolArgsDelta, ToolName: "Bash", ToolUseID: "b1",
		TextDelta: `atus"}`,
	})

	if got := string(m.toolArgsStream); got != `{"command":"git status"}` {
		t.Errorf("buffer should accumulate to full JSON; got %q", got)
	}
	if !strings.Contains(m.spinnerSub, "git status") {
		t.Errorf("spinnerSub should reflect post-accumulate value; got %q", m.spinnerSub)
	}
}
