package tui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestPersistTail_ExportKeepsParallelReadsAndSteer(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sid := "parallel-read-steer"
	if err := store.WriteHeader(sid, "test-model", "system"); err != nil {
		t.Fatal(err)
	}

	initial := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "read both files"}}}
	if err := store.AppendMessage(sid, initial); err != nil {
		t.Fatal(err)
	}
	loop := &agent.Loop{}
	loop.Restore([]llm.Message{initial})
	cursor := session.NewHistoryCursor(loop.History())
	m := &Model{loop: loop, session: store, sessionID: sid, historyCursor: cursor}

	history := []llm.Message{
		initial,
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUseID: "read-a", ToolName: "Read", ToolInput: map[string]any{"path": "/tmp/a.go"}},
			{Type: "tool_use", ToolUseID: "read-b", ToolName: "Read", ToolInput: map[string]any{"path": "/tmp/b.go"}},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "tool_result", ToolUseID: "read-a", ToolResult: "package a"},
			{Type: "tool_result", ToolUseID: "read-b", ToolResult: "package b"},
		}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "READ_BATCH_DONE"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "STEER: also include STEER_OK"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "READ_BATCH_DONE\nSTEER_OK"}}},
	}
	loop.Restore(history)
	m.persistTail()
	m.persistTail() // idempotent retry must not duplicate any message

	_, loaded, err := store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != len(history) {
		t.Fatalf("reloaded messages = %d, want %d; %#v", len(loaded), len(history), loaded)
	}
	if got := loaded[1].Content; len(got) != 2 || got[0].ToolUseID != "read-a" || got[1].ToolUseID != "read-b" {
		t.Fatalf("parallel Read tool_use blocks missing after reload: %#v", got)
	}
	if got := loaded[2].Content; len(got) != 2 || got[0].Type != "tool_result" || got[1].Type != "tool_result" {
		t.Fatalf("parallel Read tool_result blocks missing after reload: %#v", got)
	}
	if got := loaded[4].Content[0].Text; !strings.Contains(got, "STEER") {
		t.Fatalf("steering user message missing after reload: %q", got)
	}

	var exported bytes.Buffer
	if err := store.Export(sid, &exported); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"tool_use_id":"read-a"`, `"tool_use_id":"read-b"`, "STEER_OK"} {
		if !strings.Contains(exported.String(), want) {
			t.Errorf("session export missing %q:\n%s", want, exported.String())
		}
	}

	// Resume from the exported history, append one new turn, and ensure the
	// already-loaded prefix is not written a second time.
	resumed := &agent.Loop{}
	resumed.Restore(loaded)
	resumeCursor := session.NewHistoryCursor(resumed.History())
	m2 := &Model{loop: resumed, session: store, sessionID: sid, historyCursor: resumeCursor}
	resumed.AppendUser("after reload")
	m2.persistTail()
	_, loadedAgain, err := store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(loadedAgain) != len(history)+1 {
		t.Fatalf("resume persist duplicated or lost history: got %d messages, want %d", len(loadedAgain), len(history)+1)
	}
	if got := loadedAgain[len(loadedAgain)-1].Content[0].Text; got != "after reload" {
		t.Fatalf("resume tail = %q, want after reload", got)
	}
}
