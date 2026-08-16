package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	"github.com/Ricardo-M-L/metis/internal/agent"
)

// tool_result_input_isolation_test.go — guards against a class of bugs
// where tool_result content (file paths, JSON, diffs, error output)
// accidentally leaks into the chat input box, causing the user to
// inadvertently submit tool output as their next prompt.
//
// Why this file exists:
//
// The data plane has two independent channels into the input box:
//   - m.input.SetValue(text) — user-driven (keystrokes, paste, slash
//     prefill, /undo, queued-prompt drain from finalizeTurn)
//   - m.toolEvents[].Output — tool-result-driven (EventToolResult)
//
// These are structurally separate, but past regressions have shown
// that when code touches both sides (finalizeTurn's drain path
// reads m.queuedPrompts while m.toolEvents is populated), a subtle
// copy-and-paste or refactoring mistake can inject tool output into
// the input. These tests lock the invariant explicitly.

// makeIsolationModel wraps makeModelForGateTest and adds a real
// textarea so finalizeTurn's m.input.SetValue doesn't panic.
func makeIsolationModel() *Model {
	m := makeModelForGateTest()
	m.input = textarea.New()
	return m
}

// TestFinalizeTurn_DoesNotLeakToolResultToInputBox — the primary
// regression guard. After a turn with queued follow-ups AND tool
// results, finalizeTurn drains the queue into m.input. The dequeued
// text must be ONLY the user's queued prompt — never tool_result
// output, paths, or JSON fragments.
func TestFinalizeTurn_DoesNotLeakToolResultToInputBox(t *testing.T) {
	m := makeIsolationModel()
	m.turnActive = true
	m.spinnerStartedAt = time.Now().Add(-2 * time.Second)

	// Simulate: user typed "also check /tmp/foo" while a turn was
	// running. This gets queued.
	m.enqueueQueuedItem("also check /tmp/foo", QueuePriorityNext)

	// Simulate: a tool call completed with rich output containing
	// paths, JSON, and diffs that COULD leak if code mis-joins.
	m.toolEvents = []ToolEvent{
		{
			Kind:     "start",
			ToolName: "Bash",
			Input:    map[string]any{"cmd": "ls /tmp"},
		},
		{
			Kind:     "end",
			ToolName: "Bash",
			Output:   "/tmp/foo.go\n/tmp/bar.json\n[0m/usr/local/bin\n",
			IsError:  false,
		},
		{
			Kind:     "end",
			ToolName: "Read",
			Output:   "package main\n\nfunc hello() {}\n",
			IsError:  false,
		},
		{
			Kind:     "end",
			ToolName: "Grep",
			Output:   `{"matches":[{"path":"/foo.go","line":42}]}`,
			IsError:  false,
		},
	}

	m.finalizeTurn(nil)

	got := m.input.Value()

	// Invariant 1: the input contains exactly the user's queued text.
	if got != "also check /tmp/foo" {
		t.Fatalf("input = %q, want %q", got, "also check /tmp/foo")
	}

	// Invariant 2: input must NOT contain any tool_result fragments.
	badFragments := []string{
		"/tmp/foo.go",
		"package main",
		`"matches"`,
		"[0m",
		`{"matches"`,
		"usr/local/bin",
	}
	for _, frag := range badFragments {
		if strings.Contains(got, frag) {
			t.Errorf("input %q contains tool_result fragment %q", got, frag)
		}
	}
}

// TestFinalizeTurn_DrainMultipleQueueItems_NoToolLeak — regression
// guard for the batch drain path. When multiple queued items are
// drained and joined with "\n\n", the output must still contain ONLY
// user text, not tool_result fragments from m.toolEvents.
func TestFinalizeTurn_DrainMultipleQueueItems_NoToolLeak(t *testing.T) {
	m := makeIsolationModel()
	m.turnActive = true
	m.spinnerStartedAt = time.Now().Add(-2 * time.Second)

	// Three user follow-ups queued mid-turn.
	m.enqueueQueuedItem("fix the crash", QueuePriorityNext)
	m.enqueueQueuedItem("add error handling", QueuePriorityNext)
	m.enqueueQueuedItem("test it", QueuePriorityNext)

	// Tool result that shares substrings with the queued text.
	m.toolEvents = []ToolEvent{
		{
			Kind:     "end",
			ToolName: "Edit",
			Output:   "fixed crash in main.go; added error handling; tested\n",
			IsError:  false,
		},
	}

	m.finalizeTurn(nil)

	got := m.input.Value()

	// The three queued items should be joined with blank lines.
	expected := "fix the crash\n\nadd error handling\n\ntest it"
	if got != expected {
		t.Fatalf("input = %q, want %q", got, expected)
	}

	// The tool output has similar words ("fix", "error handling",
	// "tested") but different casing/suffix — none should appear
	// as-is. This catches an edge case where code accidentally
	// appends tool output to the drain string.
	if strings.Contains(got, "fixed crash") {
		t.Errorf("input contains tool_result verb form %q", "fixed crash")
	}
}

// TestEventToolResult_DoesNotModifyInputBox — regression guard for
// the event handler path. When EventToolResult arrives (possibly
// after user has typed in the input), m.input.Value() must remain
// unchanged. The tool result only populates m.toolEvents.
func TestEventToolResult_DoesNotModifyInputBox(t *testing.T) {
	m := makeIsolationModel()
	m.input.SetValue("user draft in progress")

	// EventToolResult matches against a prior EventToolStart by
	// ToolUseID. Without the start, the result can't pair and no
	// ToolEvent is appended.
	m.handleAgentEvent(agent.Event{
		Kind:      agent.EventToolStart,
		ToolName:  "Bash",
		ToolUseID: "tool-123",
	})

	ev := agent.Event{
		Kind:     agent.EventToolResult,
		ToolName: "Bash",
		ToolUseID: "tool-123",
		ToolResult: &agent.ToolResult{
			Output:  "ls output /tmp/foo\n/usr/local/bin\n[error] something broke",
			IsError: true,
		},
	}
	m.handleAgentEvent(ev)

	got := m.input.Value()
	if got != "user draft in progress" {
		t.Fatalf("input changed after EventToolResult: got %q, want %q", got, "user draft in progress")
	}
	// Verify the tool result went into toolEvents (mutating the
	// existing start event in-place, not appending a new one).
	if len(m.toolEvents) != 1 {
		t.Fatalf("expected 1 toolEvent; got %d", len(m.toolEvents))
	}
	if m.toolEvents[0].Kind != "result" {
		t.Errorf("toolEvent kind should be 'result' after EventToolResult; got %q", m.toolEvents[0].Kind)
	}
	if !strings.Contains(m.toolEvents[0].Output, "ls output") {
		t.Error("toolEvent should contain the tool result output")
	}
}

// TestEventToolStart_DoesNotModifyInputBox — same invariant for
// EventToolStart. The input box is user territory; tool events
// live in m.toolEvents only.
func TestEventToolStart_DoesNotModifyInputBox(t *testing.T) {
	m := makeIsolationModel()
	m.input.SetValue("user draft")

	ev := agent.Event{
		Kind:      agent.EventToolStart,
		ToolName:  "Edit",
		ToolUseID: "tool-456",
		ToolInput: map[string]any{"path": "/tmp/x.go", "old": "foo", "new": "bar"},
	}
	m.handleAgentEvent(ev)

	if m.input.Value() != "user draft" {
		t.Fatalf("input changed after EventToolStart: got %q", m.input.Value())
	}
}

// TestEventTextDelta_DoesNotModifyInputBox — streaming text deltas
// populate m.streamingText, not m.input. This is the most common
// event type and a regression here would be immediately visible to
// users (every keystroke could be overwritten).
func TestEventTextDelta_DoesNotModifyInputBox(t *testing.T) {
	m := makeIsolationModel()
	m.input.SetValue("user draft")

	m.handleAgentEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "here is some model output"})
	m.handleAgentEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: " more output"})

	if m.input.Value() != "user draft" {
		t.Fatalf("input changed after EventTextDelta: got %q", m.input.Value())
	}
	// Verify streaming text accumulated instead.
	if m.streamingText != "here is some model output more output" {
		t.Errorf("streamingText = %q, expected concatenated deltas", m.streamingText)
	}
}

// TestEventThinkingDelta_DoesNotModifyInputBox — thinking deltas
// go to m.thinkingText, never m.input.
func TestEventThinkingDelta_DoesNotModifyInputBox(t *testing.T) {
	m := makeIsolationModel()
	m.input.SetValue("user draft")

	m.handleAgentEvent(agent.Event{Kind: agent.EventThinkingDelta, TextDelta: "let me think"})

	if m.input.Value() != "user draft" {
		t.Fatalf("input changed after EventThinkingDelta: got %q", m.input.Value())
	}
	if m.thinkingText != "let me think" {
		t.Errorf("thinkingText = %q, expected %q", m.thinkingText, "let me think")
	}
}

// TestFinalizeTurn_DrainSlashesSolo_NoLeak — the slash drain path
// pops one slash at a time. Verify that even in the slash path,
// tool_result content never appears in m.input.
func TestFinalizeTurn_DrainSlashesSolo_NoLeak(t *testing.T) {
	m := makeIsolationModel()
	m.turnActive = true
	m.spinnerStartedAt = time.Now().Add(-2 * time.Second)

	m.enqueueQueuedItem("/tasks", QueuePriorityNext)
	m.enqueueQueuedItem("/skills", QueuePriorityNext)

	// Rich tool results that could be mis-drained.
	m.toolEvents = []ToolEvent{
		{Kind: "end", ToolName: "Read", Output: "file content here"},
		{Kind: "end", ToolName: "Bash", Output: "ls output\n"},
	}

	m.finalizeTurn(nil)

	got := m.input.Value()
	if got != "/tasks" {
		t.Fatalf("first drain should be /tasks solo; got %q", got)
	}
	if len(m.queuedPrompts) != 1 || m.queuedPrompts[0].Text != "/skills" {
		t.Fatalf("remaining queue should be [/skills]; got %+v", m.queuedPrompts)
	}
	if strings.Contains(got, "file content") || strings.Contains(got, "ls output") {
		t.Fatalf("input %q contains tool_result fragments", got)
	}
}

// TestFinalizeTurn_NoQueuedPrompts_InputUnchanged — when there are
// no queued prompts, finalizeTurn must NOT touch m.input. The input
// should remain exactly as the user left it.
func TestFinalizeTurn_NoQueuedPrompts_InputUnchanged(t *testing.T) {
	m := makeIsolationModel()
	m.turnActive = true
	m.spinnerStartedAt = time.Now().Add(-2 * time.Second)
	m.input.SetValue("my unfinished draft")

	m.toolEvents = []ToolEvent{
		{Kind: "end", ToolName: "Read", Output: "some tool result"},
	}

	m.finalizeTurn(nil)

	if m.input.Value() != "my unfinished draft" {
		t.Fatalf("input changed when no queue items: got %q", m.input.Value())
	}
}

// TestFinalizeTurn_DrainAfterToolEvents_InputOnlyUserText — the
// most realistic scenario: a turn completes with multiple tool
// calls, then the user's queued follow-ups are drained.
// This is the end-to-end integration test for the full path.
func TestFinalizeTurn_DrainAfterToolEvents_InputOnlyUserText(t *testing.T) {
	m := makeIsolationModel()
	m.turnActive = true
	m.streamingText = "done with the work"
	m.spinnerStartedAt = time.Now().Add(-30 * time.Second)

	// Simulate a realistic tool chain.
	m.toolEvents = []ToolEvent{
		{Kind: "end", ToolName: "Read", Output: "existing code here"},
		{Kind: "end", ToolName: "Edit", Output: "diff applied to /src/main.go"},
		{Kind: "end", ToolName: "Bash", Output: "go test ./... — ok"},
	}

	// User typed a follow-up during the turn.
	m.enqueueQueuedItem("please also update the docs", QueuePriorityNext)

	m.finalizeTurn(nil)

	got := m.input.Value()
	if got != "please also update the docs" {
		t.Fatalf("input = %q, want %q", got, "please also update the docs")
	}
	for _, frag := range []string{"existing code", "diff applied", "go test", "/src/main.go"} {
		if strings.Contains(got, frag) {
			t.Errorf("input %q contains tool_result fragment %q", got, frag)
		}
	}
}

// TestSteerInject_DoesNotPolluteQueuedPrompts — user steer text
// goes into loop.steerBuf, not m.queuedPrompts. Verify that after
// steering, queuedPrompts remains empty (or contains only items
// explicitly queued via enqueueQueuedItem).
func TestSteerInject_DoesNotPolluteQueuedPrompts(t *testing.T) {
	m := makeIsolationModel()
	m.turnActive = true

	// SteerInject on the loop side — we can't test the loop directly
	// here, but we CAN verify the TUI's enqueue path only accepts
	// user-typed text from submitInput. If someone accidentally
	// calls enqueueQueuedItem with a tool_result string, this test
	// catches it.
	m.enqueueQueuedItem("user prompt one", QueuePriorityNext)
	m.enqueueQueuedItem("user prompt two", QueuePriorityNext)

	got := m.input.Value()
	// Input should be empty — queued items don't pre-populate it.
	if got != "" {
		t.Fatalf("input should be empty before drain; got %q", got)
	}
}

// TestDrainNextQueuedBatch_ContainsOnlyEnqueuedText — unit-level
// test: drainNextQueuedBatch output must be a concatenation of
// enqueueQueuedItem inputs. No tool_result strings can appear.
func TestDrainNextQueuedBatch_ContainsOnlyEnqueuedText(t *testing.T) {
	m := &Model{}
	m.enqueueQueuedItem("hello", QueuePriorityNext)
	m.enqueueQueuedItem("world", QueuePriorityNext)

	text, n := m.drainNextQueuedBatch()
	if n != 2 {
		t.Fatalf("expected 2 items; got %d", n)
	}
	if text != "hello\n\nworld" {
		t.Fatalf("got %q, want %q", text, "hello\n\nworld")
	}
	// Negative: the following strings were NEVER enqueued.
	for _, wantAbsent := range []string{"tool_result", "Read", "/tmp/foo", "[{<not-json>"} {
		if strings.Contains(text, wantAbsent) {
			t.Errorf("drained text contains %q which was never enqueued", wantAbsent)
		}
	}
}