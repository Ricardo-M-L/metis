package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func histMsg(role llm.Role, text string) llm.Message {
	return llm.Message{Role: role, Content: []llm.ContentBlock{{Type: "text", Text: text}}}
}

func histToolResult(id, content string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		{Type: "tool_result", ToolUseID: id, ToolResult: content},
	}}
}

// A transcript where the interesting fact ("port 8443") is buried in an
// early message that compaction would summarize away.
func sampleTranscript() []llm.Message {
	return []llm.Message{
		histMsg(llm.RoleUser, "set up the dev server"),
		histMsg(llm.RoleAssistant, "Starting it now"),
		histToolResult("t1", "server listening on port 8443 with TLS enabled"),
		histMsg(llm.RoleUser, "now add the auth middleware"),
		histMsg(llm.RoleAssistant, "added JWT middleware to auth.go"),
		histMsg(llm.RoleUser, "what about rate limiting"),
	}
}

func runHistory(t *testing.T, msgs []llm.Message, in map[string]any) *struct {
	out     string
	isError bool
} {
	t.Helper()
	h := NewHistory(func() ([]llm.Message, error) { return msgs, nil })
	res, err := h.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return &struct {
		out     string
		isError bool
	}{res.Output, res.IsError}
}

func TestHistory_SearchFindsCompactedFact(t *testing.T) {
	r := runHistory(t, sampleTranscript(), map[string]any{
		"operation": "search",
		"query":     "port TLS",
	})
	if r.isError {
		t.Fatalf("unexpected error: %s", r.out)
	}
	var parsed struct {
		TranscriptLen int `json:"transcript_len"`
		Matches       []struct {
			Index   int    `json:"index"`
			Kind    string `json:"kind"`
			Snippet string `json:"snippet"`
		} `json:"matches"`
	}
	if err := json.Unmarshal([]byte(r.out), &parsed); err != nil {
		t.Fatalf("result not JSON: %v\n%s", err, r.out)
	}
	if parsed.TranscriptLen != 6 {
		t.Errorf("transcript_len = %d, want 6", parsed.TranscriptLen)
	}
	if len(parsed.Matches) == 0 {
		t.Fatal("no matches for a term that's in the transcript")
	}
	top := parsed.Matches[0]
	if top.Index != 2 {
		t.Errorf("top hit index = %d, want 2 (the tool_result with the port)", top.Index)
	}
	if !strings.Contains(top.Snippet, "8443") {
		t.Errorf("snippet should carry the matched content; got %q", top.Snippet)
	}
	if top.Kind != "tool_result" {
		t.Errorf("kind = %q, want tool_result", top.Kind)
	}
}

func TestHistory_AroundReturnsContext(t *testing.T) {
	r := runHistory(t, sampleTranscript(), map[string]any{
		"operation": "around",
		"index":     2,
		"before":    1,
		"after":     1,
	})
	if r.isError {
		t.Fatalf("unexpected error: %s", r.out)
	}
	// Should include messages 1, 2, 3 with 2 marked as the center.
	for _, want := range []string{"[1]", "[2]", "[3]", "» [2]", "8443"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("around output missing %q:\n%s", want, r.out)
		}
	}
	if strings.Contains(r.out, "[0]") || strings.Contains(r.out, "[4]") {
		t.Errorf("around window leaked outside before/after bounds:\n%s", r.out)
	}
}

func TestHistory_AroundClampsBounds(t *testing.T) {
	r := runHistory(t, sampleTranscript(), map[string]any{
		"operation": "around", "index": 0, "before": 5, "after": 1,
	})
	if r.isError {
		t.Fatalf("unexpected error: %s", r.out)
	}
	if !strings.Contains(r.out, "[0]") || strings.Contains(r.out, "[-1]") {
		t.Errorf("around at index 0 should clamp lo to 0:\n%s", r.out)
	}
}

func TestHistory_Errors(t *testing.T) {
	// out-of-range index
	r := runHistory(t, sampleTranscript(), map[string]any{"operation": "around", "index": 99})
	if !r.isError || !strings.Contains(r.out, "out of range") {
		t.Errorf("expected out-of-range error; got %q (isError=%v)", r.out, r.isError)
	}
	// missing query
	r = runHistory(t, sampleTranscript(), map[string]any{"operation": "search"})
	if !r.isError || !strings.Contains(r.out, "required") {
		t.Errorf("expected missing-query error; got %q", r.out)
	}
	// unknown operation
	r = runHistory(t, sampleTranscript(), map[string]any{"operation": "frobnicate"})
	if !r.isError || !strings.Contains(r.out, "unknown operation") {
		t.Errorf("expected unknown-operation error; got %q", r.out)
	}
}

func TestHistory_NoLoaderOrEmpty(t *testing.T) {
	// nil loader
	h := NewHistory(nil)
	res, _ := h.Execute(context.Background(), map[string]any{"operation": "search", "query": "x"})
	if !res.IsError || !strings.Contains(res.Output, "unavailable") {
		t.Errorf("nil loader should report unavailable; got %q", res.Output)
	}
	// empty transcript → clean "no matches", not a crash
	r := runHistory(t, nil, map[string]any{"operation": "search", "query": "anything"})
	if r.isError || !strings.Contains(r.out, "no messages matched") {
		t.Errorf("empty transcript should yield a clean no-match; got %q (isError=%v)", r.out, r.isError)
	}
}
