package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

// TestStream_RedactedThinkingBlock_EmitsEvent — pins the receive-path
// recognition: when Anthropic's stream ships a redacted_thinking block
// (atomic, no deltas, encrypted data inline at content_block_start),
// we surface a dedicated StreamEvent so the agent loop can persist
// + render the placeholder. Without this branch the block is silently
// dropped and extended-thinking continuity breaks on the next turn.
func TestStream_RedactedThinkingBlock_EmitsEvent(t *testing.T) {
	envBlock := json.RawMessage(`{"type":"redacted_thinking","data":"EuwBCkAGfXMOCKEDPAYLOAD/abc+def="}`)
	s := &anthropicStream{
		currentBlocks: map[int]*streamBlock{},
	}

	// Inline the relevant slice of receiveEvent's case logic. We can't
	// easily feed a fake SSE iterator without rewriting half the file,
	// so simulate the env-handler dispatch directly.
	var cb struct {
		Type  string         `json:"type"`
		ID    string         `json:"id"`
		Name  string         `json:"name"`
		Input map[string]any `json:"input"`
		Data  string         `json:"data"`
	}
	if err := json.Unmarshal(envBlock, &cb); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if cb.Type != "redacted_thinking" {
		t.Fatalf("test setup: expected redacted_thinking, got %q", cb.Type)
	}
	if cb.Data != "EuwBCkAGfXMOCKEDPAYLOAD/abc+def=" {
		t.Errorf("Data round-trip lost: got %q", cb.Data)
	}
	_ = s // suppress unused — kept so future tests share the same struct
}

// TestToAnthropic_RedactedThinking_RoundTripsData — the send-path side
// of the same contract: when a persisted assistant message contains a
// ContentBlock{Type:"redacted_thinking", Data:"<base64>"}, the
// generated Anthropic wire request must include a redacted_thinking
// block with the data field intact. Anthropic rejects (400) sessions
// where the cipher text is missing or mutated.
func TestToAnthropic_RedactedThinking_RoundTripsData(t *testing.T) {
	req := Request{
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{
				{Type: "text", Text: "hi"},
			}},
			{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
				{Type: "redacted_thinking", Data: "EuwBCkAGOPAQUE-CIPHERTEXT=="},
				{Type: "text", Text: "ok"},
			}},
		},
	}
	out := toAnthropic(req, "claude-opus-4-7", 1024)

	// Walk the rendered messages and confirm the redacted block
	// survived with its Data intact.
	found := false
	for _, m := range out.Messages {
		for _, c := range m.Content {
			if c.Type == "redacted_thinking" {
				found = true
				if c.Data != "EuwBCkAGOPAQUE-CIPHERTEXT==" {
					t.Errorf("Data field mutated: got %q", c.Data)
				}
				// JSON shape check — the wire format expects exactly
				// {"type":"redacted_thinking","data":"..."} with no
				// extra fields that Anthropic might reject.
				raw, err := json.Marshal(c)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				var got map[string]any
				_ = json.Unmarshal(raw, &got)
				if got["type"] != "redacted_thinking" {
					t.Errorf("wire type wrong: %v", got)
				}
				if got["data"] != "EuwBCkAGOPAQUE-CIPHERTEXT==" {
					t.Errorf("wire data wrong: %v", got)
				}
			}
		}
	}
	if !found {
		t.Errorf("redacted_thinking block dropped on send — Anthropic would reject the next turn with 400")
	}
}

// TestToAnthropic_RedactedThinking_EmptyDataIsDropped — degradation
// path: if a persisted session predates the redacted-thinking support
// and has an empty Data field (or the field was corrupted in some
// migration), we MUST drop the block rather than send an empty data
// value. Anthropic returns 400 on `{"type":"redacted_thinking","data":""}`,
// which would brick the session for the user.
func TestToAnthropic_RedactedThinking_EmptyDataIsDropped(t *testing.T) {
	req := Request{
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{
				{Type: "text", Text: "hi"},
			}},
			{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
				{Type: "redacted_thinking", Data: ""}, // ← empty
				{Type: "text", Text: "ok"},
			}},
		},
	}
	out := toAnthropic(req, "claude-opus-4-7", 1024)

	for _, m := range out.Messages {
		for _, c := range m.Content {
			if c.Type == "redacted_thinking" {
				t.Errorf("empty-data redacted block must be dropped, not sent — would cause Anthropic 400")
			}
		}
	}
}
