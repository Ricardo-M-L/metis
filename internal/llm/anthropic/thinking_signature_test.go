package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func TestToAnthropic_ThinkingRequiresAndRoundTripsSignature(t *testing.T) {
	req := Request{Messages: []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
			{
				Type: "thinking",
				Text: "signed reasoning",
				ProviderHint: map[string]string{
					ThinkingSignatureHint: "opaque-signature",
				},
			},
			{Type: "thinking", Text: "old unsigned reasoning"},
			{Type: "text", Text: "answer"},
		}},
	}}

	wire := toAnthropic(req, "claude-sonnet-4-6", 1024)
	if len(wire.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(wire.Messages))
	}
	if got := len(wire.Messages[0].Content); got != 2 {
		t.Fatalf("wire content blocks = %d, want signed thinking + text; blocks=%+v", got, wire.Messages[0].Content)
	}
	thinking := wire.Messages[0].Content[0]
	if thinking.Type != "thinking" || thinking.Thinking != "signed reasoning" || thinking.Signature != "opaque-signature" {
		t.Fatalf("thinking block = %+v, want exact text/signature pair", thinking)
	}
	raw, err := json.Marshal(thinking)
	if err != nil {
		t.Fatalf("marshal thinking: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal thinking: %v", err)
	}
	if fields["thinking"] != "signed reasoning" || fields["signature"] != "opaque-signature" {
		t.Fatalf("wire JSON lost thinking signature: %s", raw)
	}
	if _, exists := fields["text"]; exists {
		t.Fatalf("thinking wire block must use thinking, not text: %s", raw)
	}
}

func TestToAnthropic_SignedEmptyThinkingKeepsRequiredEmptyField(t *testing.T) {
	req := Request{Messages: []provider.Message{{
		Role: provider.RoleAssistant,
		Content: []provider.ContentBlock{{
			Type: "thinking",
			ProviderHint: map[string]string{
				ThinkingSignatureHint: "signature-for-empty-thinking",
			},
		}},
	}}}

	wire := toAnthropic(req, "claude-sonnet-4-6", 1024)
	if len(wire.Messages) != 1 || len(wire.Messages[0].Content) != 1 {
		t.Fatalf("signed empty thinking was dropped: %+v", wire.Messages)
	}
	raw, err := json.Marshal(wire.Messages[0].Content[0])
	if err != nil {
		t.Fatalf("marshal thinking: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal thinking: %v", err)
	}
	thinking, exists := fields["thinking"]
	if !exists || thinking != "" {
		t.Fatalf("required empty thinking field missing or changed: %s", raw)
	}
	if fields["signature"] != "signature-for-empty-thinking" {
		t.Fatalf("signature missing: %s", raw)
	}
}

func TestFromAnthropic_PreservesThinkingSignature(t *testing.T) {
	resp := anthropicResp{Content: []anthropicContent{
		{Type: "thinking", Thinking: "reasoning", Signature: "sig-123"},
		{Type: "redacted_thinking", Data: "encrypted-reasoning"},
		{Type: "text", Text: "done"},
	}}

	got := fromAnthropic(resp)
	if len(got.Content) != 3 {
		t.Fatalf("content = %+v, want thinking + redacted + text", got.Content)
	}
	thinking := got.Content[0]
	if thinking.Type != "thinking" || thinking.Text != "reasoning" || thinking.ProviderHint[ThinkingSignatureHint] != "sig-123" {
		t.Fatalf("thinking response block lost signature: %+v", thinking)
	}
	if got.Content[1].Type != "redacted_thinking" || got.Content[1].Data != "encrypted-reasoning" {
		t.Fatalf("redacted thinking response block lost data: %+v", got.Content[1])
	}
}

func TestCanReplayAssistantBlock_ThinkingNeedsOriginalSignature(t *testing.T) {
	tests := []struct {
		name  string
		block provider.ContentBlock
		want  bool
	}{
		{name: "signed thinking", block: provider.ContentBlock{Type: "thinking", Text: "x", ProviderHint: map[string]string{ThinkingSignatureHint: "sig"}}, want: true},
		{name: "unsigned thinking", block: provider.ContentBlock{Type: "thinking", Text: "x"}, want: false},
		{name: "empty signature", block: provider.ContentBlock{Type: "thinking", Text: "x", ProviderHint: map[string]string{ThinkingSignatureHint: ""}}, want: false},
		{name: "redacted payload", block: provider.ContentBlock{Type: "redacted_thinking", Data: "ciphertext"}, want: true},
		{name: "empty redacted payload", block: provider.ContentBlock{Type: "redacted_thinking"}, want: false},
		{name: "text", block: provider.ContentBlock{Type: "text", Text: "answer"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanReplayAssistantBlock(tt.block); got != tt.want {
				t.Fatalf("CanReplayAssistantBlock(%+v) = %t, want %t", tt.block, got, tt.want)
			}
		})
	}
}

func TestAnthropicStream_AccumulatesThinkingSignatureWithoutRenderingIt(t *testing.T) {
	body := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"inspect files"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque-"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signature"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_stop
data: {"type":"message_stop"}

`
	events := drainStream(t, body)
	var thinking, signature *StreamEvent
	for i := range events {
		switch events[i].Type {
		case "thinking_delta":
			thinking = &events[i]
		case "thinking_signature":
			signature = &events[i]
		}
	}
	if thinking == nil || thinking.TextDelta != "inspect files" {
		t.Fatalf("thinking delta missing: %+v", events)
	}
	if signature == nil || signature.ProviderHint[ThinkingSignatureHint] != "opaque-signature" {
		t.Fatalf("thinking signature missing: %+v", events)
	}
	if signature.TextDelta != "" {
		t.Fatalf("opaque signature leaked into renderable text: %q", signature.TextDelta)
	}
}

func TestAnthropicStream_PreservesPrefilledThinkingAndSignature(t *testing.T) {
	body := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"prefilled reasoning","signature":"prefilled-signature"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_stop
data: {"type":"message_stop"}

`
	events := drainStream(t, body)
	var thinking, signature *StreamEvent
	for i := range events {
		switch events[i].Type {
		case "thinking_delta":
			thinking = &events[i]
		case "thinking_signature":
			signature = &events[i]
		}
	}
	if thinking == nil || thinking.TextDelta != "prefilled reasoning" {
		t.Fatalf("prefilled thinking missing: %+v", events)
	}
	if signature == nil || signature.ProviderHint[ThinkingSignatureHint] != "prefilled-signature" {
		t.Fatalf("prefilled signature missing: %+v", events)
	}
}
