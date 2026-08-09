package tui

// Phase-C smoke tests. We exercise the pure-string handlers (cmdCopy
// with no history, cmdOutputStyle dispatcher, cmdBreakCache help text,
// cmdSecurityReview prompt shape, cmdFeedback aliasing). The git +
// gh subprocess paths in cmdCommitPushPR aren't worth a fake-git
// fixture: cmdCommitPushPR now loads a permissioned agent workflow prompt
// instead of running git/gh directly.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestCopy_NoActiveSession(t *testing.T) {
	// REPL with nil Loop covers the "no session" guard. Useful as the
	// canonical "calling cmdCopy before chat is wired up" path.
	r := &REPL{}
	out := cmdCopy(r, "")
	if !strings.Contains(out, "no active session") {
		t.Errorf("expected 'no active session'; got: %q", out)
	}
}

func TestCommitPushPRLoadsPermissionedWorkflowPrompt(t *testing.T) {
	var inserted string
	r := &REPL{InsertInput: func(s string) { inserted = s }}
	out := cmdCommitPushPR(r, "fix slash routing")
	if !strings.Contains(out, "loaded into input") {
		t.Fatalf("output=%q", out)
	}
	for _, want := range []string{"git status", "Do not stage unrelated", "fix slash routing", "never force-push"} {
		if !strings.Contains(inserted, want) {
			t.Fatalf("workflow prompt missing %q:\n%s", want, inserted)
		}
	}
}

func TestCopy_BadCount(t *testing.T) {
	r := &REPL{}
	out := cmdCopy(r, "abc")
	if !strings.Contains(out, "usage:") {
		t.Errorf("non-numeric arg should show usage; got: %q", out)
	}
}

func TestOutputStyle_Default(t *testing.T) {
	r := &REPL{}
	out := cmdOutputStyle(r, "")
	if !strings.Contains(out, "full") {
		t.Errorf("default state should be 'full'; got:\n%s", out)
	}
}

func TestOutputStyle_Switch(t *testing.T) {
	r := &REPL{UseMarkdown: true}
	cmdOutputStyle(r, "minimal")
	if r.outputStyle != "minimal" {
		t.Errorf("outputStyle should be 'minimal'; got %q", r.outputStyle)
	}
	if r.UseMarkdown {
		t.Errorf("minimal should disable markdown")
	}
	cmdOutputStyle(r, "full")
	if !r.UseMarkdown {
		t.Errorf("full should re-enable markdown")
	}
}

func TestOutputStyle_UnknownArg(t *testing.T) {
	r := &REPL{}
	out := cmdOutputStyle(r, "loud")
	if !strings.Contains(out, "unknown") {
		t.Errorf("unknown arg should error; got: %q", out)
	}
}

func TestOutputStyle_TUIBridgeChangesRenderedTranscript(t *testing.T) {
	m := newSlashTestModel(t)
	thinking := Message{Role: "thinking", Content: "private reasoning marker", Timestamp: time.Now()}
	assistant := Message{Role: "assistant", Content: "**raw-bold-marker**", Timestamp: time.Now().Add(time.Millisecond)}
	tool := ToolEvent{
		ToolName:  "Bash",
		Kind:      "result",
		Output:    "summary line\nbody-detail-marker",
		StartTime: time.Now().Add(2 * time.Millisecond),
	}

	render := func() string {
		m.messages = []Message{thinking, assistant}
		m.toolEvents = []ToolEvent{tool}
		var out strings.Builder
		for _, item := range m.buildChatItems() {
			out.WriteString(item.Render(80))
		}
		return stripANSI(out.String())
	}
	submit := func(command string) {
		m.input.SetValue(command)
		pressEnter(t, m)
	}

	submit("/output-style streamlined")
	if m.outputStyle != outputStyleStreamlined {
		t.Fatalf("TUI outputStyle = %q, want streamlined", m.outputStyle)
	}
	streamlined := render()
	if strings.Contains(streamlined, "private reasoning marker") {
		t.Fatalf("streamlined output leaked thinking:\n%s", streamlined)
	}
	if strings.Contains(streamlined, "body-detail-marker") {
		t.Fatalf("streamlined output did not collapse tool body:\n%s", streamlined)
	}
	if !strings.Contains(streamlined, "bash") {
		t.Fatalf("streamlined output hid the tool summary entirely:\n%s", streamlined)
	}

	submit("/output-style minimal")
	if m.outputStyle != outputStyleMinimal {
		t.Fatalf("TUI outputStyle = %q, want minimal", m.outputStyle)
	}
	minimal := render()
	if !strings.Contains(minimal, "**raw-bold-marker**") {
		t.Fatalf("minimal output still applied Markdown instead of preserving literal text:\n%s", minimal)
	}
	if strings.Contains(minimal, "private reasoning marker") || strings.Contains(minimal, "body-detail-marker") {
		t.Fatalf("minimal output was not streamlined:\n%s", minimal)
	}

	submit("/output-style full")
	if m.outputStyle != outputStyleFull {
		t.Fatalf("TUI outputStyle = %q, want full", m.outputStyle)
	}
	full := render()
	if !strings.Contains(full, "private reasoning marker") {
		t.Fatalf("full output did not restore thinking:\n%s", full)
	}
	if !strings.Contains(full, "body-detail-marker") {
		t.Fatalf("full output did not restore the tool body:\n%s", full)
	}
}

func TestCollectSessionInsights_UsesTypedJSONLEnvelope(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeader("recent-a", "model-a", "system"); err != nil {
		t.Fatal(err)
	}
	// This superseded message remains physically present in JSONL. A raw-line
	// counter would include it; Store.Load applies the history_replace below.
	if err := store.AppendMessage("recent-a", llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "superseded"}},
	}); err != nil {
		t.Fatal(err)
	}
	logical := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "run"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUseID: "tool-1", ToolName: "Read", ToolInput: map[string]any{"path": "a.go"}},
			{Type: "tool_use", ToolUseID: "tool-2", ToolName: "Bash", ToolInput: map[string]any{"command": "false"}},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "tool_result", ToolUseID: "tool-1", ToolResult: "ok"},
			{Type: "tool_result", ToolUseID: "tool-2", ToolResult: "exit 1", IsError: true},
		}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "done"}}},
	}
	cursor := session.NewHistoryCursor(nil)
	if err := store.ReplaceHistoryAndMark("recent-a", logical, &cursor); err != nil {
		t.Fatal(err)
	}

	if err := store.WriteHeader("recent-b", "model-b", "system"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("recent-b", llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.WriteHeader("old", "old-model", "system"); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	oldPath := filepath.Join(store.Dir, "old.jsonl")
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	stats, err := collectSessionInsights(store, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if stats.sessions != 2 || stats.messages != 5 || stats.toolCalls != 2 || stats.toolErrors != 1 {
		t.Fatalf("unexpected insights: %+v", stats)
	}
	if stats.modelMix["model-a"] != 1 || stats.modelMix["model-b"] != 1 || stats.modelMix["old-model"] != 0 {
		t.Fatalf("unexpected model mix: %#v", stats.modelMix)
	}

	out := stripANSI(cmdInsights(&REPL{Session: store}, "--days=7"))
	for _, want := range []string{"sessions", "messages", "tool calls", "tool errors", "model-a", "model-b"} {
		if !strings.Contains(out, want) {
			t.Fatalf("/insights output missing %q:\n%s", want, out)
		}
	}
}

func TestBreakCache_RendersHelp(t *testing.T) {
	r := &REPL{}
	out := cmdBreakCache(r, "")
	if !strings.Contains(out, "/compact") || !strings.Contains(out, "/clear") {
		t.Errorf("break-cache should mention /compact and /clear; got:\n%s", out)
	}
}

func TestSecurityReview_DefaultTarget(t *testing.T) {
	r := &REPL{}
	out := cmdSecurityReview(r, "")
	if !strings.Contains(out, "OWASP") && !strings.Contains(out, "SQL injection") {
		t.Errorf("security-review should mention OWASP or specific class; got: %q", out)
	}
	if !strings.Contains(out, "staged changes") {
		t.Errorf("default target should be staged changes; got: %q", out)
	}
}

func TestSecurityReview_ExplicitTarget(t *testing.T) {
	r := &REPL{}
	out := cmdSecurityReview(r, "internal/auth/")
	if !strings.Contains(out, "internal/auth/") {
		t.Errorf("explicit target should appear in prompt; got: %q", out)
	}
}

func TestFeedback_AliasesBug(t *testing.T) {
	r := &REPL{}
	feedbackOut := cmdFeedback(r, "")
	bugOut := cmdBug(r, "")
	if feedbackOut != bugOut {
		t.Errorf("/feedback should produce identical output to /bug;\nfeedback: %q\nbug: %q", feedbackOut, bugOut)
	}
}
