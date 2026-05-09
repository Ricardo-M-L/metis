package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests pin the MarshalJSON behaviour for tool_use content.
// MiniMax's anthropic-compat endpoint rejects requests where a
// tool_use block lacks an `input` field with code (2013); previously
// metis serialized empty inputs as missing fields (`omitempty`
// dropped both nil and empty maps). The custom MarshalJSON now
// guarantees `"input": {}` is present on every tool_use block.

func TestAnthropicContent_ToolUse_NilInputBecomesEmptyObject(t *testing.T) {
	c := anthropicContent{Type: "tool_use", ID: "call_1", Name: "MetisInfo", Input: nil}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"input":{}`) {
		t.Errorf("expected literal input:{}, got %s", b)
	}
}

func TestAnthropicContent_ToolUse_EmptyMapInputBecomesEmptyObject(t *testing.T) {
	c := anthropicContent{Type: "tool_use", ID: "call_1", Name: "MetisInfo", Input: map[string]any{}}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"input":{}`) {
		t.Errorf("expected literal input:{} on empty map, got %s", b)
	}
}

func TestAnthropicContent_ToolUse_NonEmptyInputPreserved(t *testing.T) {
	c := anthropicContent{
		Type: "tool_use", ID: "call_1", Name: "Bash",
		Input: map[string]any{"command": "ls"},
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"command":"ls"`) {
		t.Errorf("expected command preserved, got %s", b)
	}
}

func TestAnthropicContent_Text_StillOmitsInput(t *testing.T) {
	// text blocks should NOT grow a stray "input":{} field.
	c := anthropicContent{Type: "text", Text: "hello"}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), `"input"`) {
		t.Errorf("text block must not carry input field, got %s", b)
	}
}

func TestAnthropicContent_ToolResult_DoesNotGetInputField(t *testing.T) {
	// tool_result blocks should also not get input:{}.
	c := anthropicContent{Type: "tool_result", ToolUseID: "call_1", Content: "ok"}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), `"input"`) {
		t.Errorf("tool_result must not carry input field, got %s", b)
	}
}

func TestAnthropicContent_ToolUse_PreservesCacheControl(t *testing.T) {
	c := anthropicContent{
		Type: "tool_use", ID: "x", Name: "y", Input: nil,
		CacheControl: &anthropicCacheControl{Type: "ephemeral"},
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"cache_control":{"type":"ephemeral"}`) {
		t.Errorf("cache_control lost: %s", s)
	}
	if !strings.Contains(s, `"input":{}`) {
		t.Errorf("input not emitted alongside cache_control: %s", s)
	}
}

func TestAnthropicContent_RoundTrip_ToolUseWithEmptyInput(t *testing.T) {
	// Round-trip: marshal then unmarshal, verify Input parses back to
	// empty map (not nil), and that downstream consumers can rely on
	// the field being present.
	orig := anthropicContent{Type: "tool_use", ID: "c1", Name: "T", Input: nil}
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got anthropicContent
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Type != "tool_use" {
		t.Errorf("Type: %q", got.Type)
	}
	if got.Input == nil {
		t.Errorf("Input nil after round-trip — should be empty map")
	}
	if len(got.Input) != 0 {
		t.Errorf("Input should be empty after round-trip, got %v", got.Input)
	}
}
