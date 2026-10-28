package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func conversationText(text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: text}}}
}

func newConversationTranscript(t *testing.T) (string, string, *SubAgentTranscript) {
	t.Helper()
	dir := t.TempDir()
	id := "agt-conversation"
	tr, err := NewSubAgentTranscript(dir, id, NewSubAgentHeader(id, "fixture-model", "parent-session", "reader", "", "default"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return dir, id, tr
}

func writeConversationEntries(t *testing.T, dir, id string, entries ...subagentEntry) string {
	t.Helper()
	var data bytes.Buffer
	for _, entry := range entries {
		if err := json.NewEncoder(&data).Encode(entry); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, SubAgentTranscriptDirname, id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSubAgentConversationPreservesFullContentAndTerminal(t *testing.T) {
	t.Parallel()
	dir, id, tr := newConversationTranscript(t)
	longResult := "BEGIN\n" + strings.Repeat("完整工具输出 <value>\n", 6000) + "\nEND"
	thinking := "分析开始\n" + strings.Repeat("thinking detail\n", 2500) + "分析结束"
	messages := []llm.Message{
		conversationText("Read the complete tool output and explain the result."),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "thinking", Text: thinking, ProviderHint: map[string]string{"anthropic.thinking_signature": "fixture-signature"}},
			{Type: "tool_use", ToolUseID: "tool-1", TraceCallID: "call-1", ToolName: "Bash", ToolInput: map[string]any{"command": "printf fixture", "description": "Read fixture"}},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "tool-1", TraceCallID: "call-1", ToolResult: longResult, IsError: true}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "## Result\n\nThe tool returned an error."}}},
	}
	for _, message := range messages {
		if err := tr.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	terminal := SubAgentTerminal{Status: "failed", Background: true, EndedAt: time.Date(2026, time.October, 28, 12, 0, 0, 0, time.UTC), ExitError: "fixture tool failed"}
	if err := tr.AppendTerminal(terminal); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := LoadSubAgentConversation(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.ID != id || got.Header.SubAgentOf != "parent-session" || got.Header.TeammateName != "reader" {
		t.Fatalf("wrong conversation identity: %#v", got.Header)
	}
	if !reflect.DeepEqual(got.Messages, messages) {
		t.Fatal("conversation changed or truncated thinking, tool arguments, long result, error, or Markdown")
	}
	if !reflect.DeepEqual(got.Terminal, &terminal) {
		t.Fatalf("terminal=%#v, want %#v", got.Terminal, terminal)
	}
	if got.IncompleteTail || got.ContextResets != 0 {
		t.Fatalf("unexpected incomplete/reset metadata: tail=%v resets=%d", got.IncompleteTail, got.ContextResets)
	}
	if got.Revision == "" {
		t.Fatal("conversation revision must identify the saved transcript")
	}
	again, err := LoadSubAgentConversation(dir, id)
	if err != nil || again.Revision != got.Revision {
		t.Fatalf("unchanged transcript revision changed: first=%q second=%#v err=%v", got.Revision, again, err)
	}
}

func TestSubAgentConversationKeepsLegitimateRepeatedMessages(t *testing.T) {
	t.Parallel()
	dir, id, tr := newConversationTranscript(t)
	repeated := conversationText("Please retry the same request.")
	if err := tr.Checkpoint([]llm.Message{repeated, repeated}); err != nil {
		t.Fatal(err)
	}
	if err := tr.Checkpoint([]llm.Message{repeated, repeated, repeated}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSubAgentConversation(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if want := []llm.Message{repeated, repeated, repeated}; !reflect.DeepEqual(got.Messages, want) {
		t.Fatalf("identical messages at distinct positions must survive: got %d messages", len(got.Messages))
	}
	if got.ContextResets != 0 {
		t.Fatalf("ordinary repeated suffix became a context reset: %d", got.ContextResets)
	}
}

func TestSubAgentConversationRetainsEditedCompactedAndEmptyResetSegments(t *testing.T) {
	t.Parallel()
	dir, id, tr := newConversationTranscript(t)
	initial := []llm.Message{conversationText("first"), conversationText("second")}
	appended := append(append([]llm.Message{}, initial...), conversationText("third"))
	edited := []llm.Message{conversationText("first"), conversationText("edited second"), conversationText("third")}
	compacted := []llm.Message{conversationText("compacted summary")}
	afterEmpty := []llm.Message{conversationText("repeat"), conversationText("repeat")}
	for _, history := range [][]llm.Message{initial, appended, edited, compacted, {}, afterEmpty} {
		if err := tr.Checkpoint(history); err != nil {
			t.Fatal(err)
		}
	}
	got, err := LoadSubAgentConversation(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	wantMessages := append([]llm.Message{}, appended...)
	wantMessages = append(wantMessages, edited...)
	wantMessages = append(wantMessages, compacted...)
	wantMessages = append(wantMessages, afterEmpty...)
	if !reflect.DeepEqual(got.Messages, wantMessages) {
		t.Fatalf("reset discarded recorded history or deduplicated reset context: got %d messages, want %d", len(got.Messages), len(wantMessages))
	}
	wantSegments := []SubAgentConversationSegment{
		{StartIndex: 0, Reason: "initial"},
		{StartIndex: 3, Reason: "context_reset"},
		{StartIndex: 6, Reason: "context_reset"},
		{StartIndex: 7, Reason: "context_reset"},
	}
	if !reflect.DeepEqual(got.Segments, wantSegments) || got.ContextResets != 3 {
		t.Fatalf("segments=%#v resets=%d, want segments=%#v resets=3", got.Segments, got.ContextResets, wantSegments)
	}
	// Resume history keeps its existing meaning: display retention cannot alter
	// the producer's exact current-context replay or force old context into it.
	resumed, err := LoadSubAgentSnapshot(dir, id)
	if err != nil || !reflect.DeepEqual(resumed.Messages, afterEmpty) {
		t.Fatalf("resume context was changed by display replay: snapshot=%#v err=%v", resumed, err)
	}
}

func TestSubAgentConversationIgnoresIncompleteFinalRecord(t *testing.T) {
	t.Parallel()
	dir, id, tr := newConversationTranscript(t)
	want := []llm.Message{conversationText("saved before crash"), conversationText("another complete message")}
	if err := tr.Checkpoint(want); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(tr.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"checkpoint","checkpoint":{"offset":2,"messages":[`); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(tr.Path())
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadSubAgentConversation(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IncompleteTail || !reflect.DeepEqual(got.Messages, want) {
		t.Fatalf("incomplete record corrupted saved messages: tail=%v messages=%#v", got.IncompleteTail, got.Messages)
	}
	after, err := os.ReadFile(tr.Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("read-only conversation reader changed crash evidence: err=%v", err)
	}
}

func TestSubAgentConversationRejectsUnsafeAgentIDs(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"", "..", "../outside", "nested/child", `nested\child`, "agt\nchild", "agt\rchild", "agt\tchild", "agt\x00child"} {
		t.Run(id, func(t *testing.T) {
			dir := t.TempDir()
			if !strings.ContainsRune(id, '\x00') {
				// Put a valid matching header at the unsafe target, so an error
				// cannot pass merely because the constructed filename is missing.
				header := NewSubAgentHeader(id, "fixture", "parent", "reader", "", "default")
				writeConversationEntries(t, dir, id, subagentEntry{Type: "header", Header: &header})
			}
			if _, err := LoadSubAgentConversation(dir, id); err == nil {
				t.Fatalf("unsafe agent id %q was accepted", id)
			}
		})
	}
}

func TestSubAgentConversationRejectsInvalidCheckpointOffsets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		offset int
		reset  bool
	}{
		{name: "suffix_rewinds_history", offset: 0},
		{name: "suffix_skips_history", offset: 2},
		{name: "negative_offset", offset: -1},
		{name: "reset_must_start_at_zero", offset: 1, reset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			id := "agt-invalid-offset"
			header := NewSubAgentHeader(id, "fixture", "parent", "reader", "", "default")
			message := conversationText("existing history")
			writeConversationEntries(t, dir, id,
				subagentEntry{Type: "header", Header: &header},
				subagentEntry{Type: "message", Message: &message},
				subagentEntry{Type: "checkpoint", Checkpoint: &subagentHistoryCheckpoint{Offset: tc.offset, Reset: tc.reset, Messages: []llm.Message{conversationText("must not silently repair")}}},
			)
			if _, err := LoadSubAgentConversation(dir, id); err == nil {
				t.Fatal("invalid checkpoint offset was silently accepted")
			}
		})
	}
}

func TestSubAgentConversationRejectsRunReplacementWhileReading(t *testing.T) {
	for _, atomicReplace := range []bool{false, true} {
		t.Run(fmt.Sprintf("atomic=%v", atomicReplace), func(t *testing.T) {
			dir, id, tr := newConversationTranscript(t)
			if err := tr.Checkpoint([]llm.Message{conversationText(strings.Repeat("old owner body", 4000))}); err != nil {
				t.Fatal(err)
			}
			if err := tr.Close(); err != nil {
				t.Fatal(err)
			}
			got, err := loadSubAgentConversation(dir, id, func() {
				header := NewSubAgentHeader(id, "fixture", "new-parent", "new-run", "", "default")
				var encoded bytes.Buffer
				_ = json.NewEncoder(&encoded).Encode(subagentEntry{Type: "header", Header: &header})
				message := conversationText(strings.Repeat("new owner body", 4000))
				_ = json.NewEncoder(&encoded).Encode(subagentEntry{Type: "message", Message: &message})
				path := filepath.Join(dir, SubAgentTranscriptDirname, id+".jsonl")
				if atomicReplace {
					if err := os.WriteFile(path+".new", encoded.Bytes(), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(path+".new", path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
			})
			if err == nil || got != nil {
				t.Fatalf("replacement must not expose old-owner/new-body history: got=%+v err=%v", got, err)
			}
		})
	}
}
