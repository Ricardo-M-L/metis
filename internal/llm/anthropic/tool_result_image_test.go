package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

// TestToAnthropic_ToolResultMultiPartImage — when a tool returns a
// vision payload, dispatch hands the loop a ContentBlock with
// ToolResultBlocks=[{text}, {image}]. The Anthropic wire shape must
// be content=[{type:"text",...}, {type:"image",source:{...}}] —
// NOT the string form. Without this fan-out the model receives the
// textual summary only and has no way to actually see the bytes.
func TestToAnthropic_ToolResultMultiPartImage(t *testing.T) {
	req := Request{
		Model: "claude-opus-4-7",
		Messages: []provider.Message{
			{
				Role: provider.RoleUser,
				Content: []provider.ContentBlock{
					{
						Type:       "tool_result",
						ToolUseID:  "toolu_test",
						ToolResult: "ViewImage: /tmp/x.png (123 bytes, image/png)",
						ToolResultBlocks: []provider.ContentBlock{
							{Type: "text", Text: "ViewImage: /tmp/x.png (123 bytes, image/png)"},
							{
								Type:      "image",
								MediaType: "image/png",
								Data:      "iVBORw0KGgoAAAANSUhEUg==",
							},
						},
					},
				},
			},
		},
	}
	body := toAnthropic(req, "claude-opus-4-7", 1024)
	if len(body.Messages) != 1 || len(body.Messages[0].Content) != 1 {
		t.Fatalf("expected 1 message with 1 content block; got %d / %d",
			len(body.Messages), len(body.Messages[0].Content))
	}
	tr := body.Messages[0].Content[0]
	if tr.Type != "tool_result" {
		t.Fatalf("Type = %q, want tool_result", tr.Type)
	}
	// Content must be a []anthropicContent slice now, not a string.
	sub, ok := tr.Content.([]anthropicContent)
	if !ok {
		t.Fatalf("tool_result.Content type = %T, want []anthropicContent (multi-part body)", tr.Content)
	}
	if len(sub) != 2 {
		t.Fatalf("sub-block count = %d, want 2", len(sub))
	}
	if sub[0].Type != "text" || !strings.Contains(sub[0].Text, "ViewImage") {
		t.Errorf("sub[0] = %+v, want text part summarising the result", sub[0])
	}
	if sub[1].Type != "image" || sub[1].Source == nil {
		t.Fatalf("sub[1] = %+v, want image block with non-nil Source", sub[1])
	}
	if sub[1].Source.MediaType != "image/png" {
		t.Errorf("Source.MediaType = %q, want image/png", sub[1].Source.MediaType)
	}
	if sub[1].Source.Data == "" {
		t.Error("Source.Data is empty; base64 payload must be carried through verbatim")
	}

	// Round-trip JSON to confirm the wire shape Anthropic actually
	// receives. Anthropic's API rejects tool_result with bad shape
	// (e.g. missing source.type) at 400 well before the model sees
	// anything, so this is the load-bearing assertion.
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, `"type":"tool_result"`) {
		t.Errorf("missing tool_result type tag in: %s", got)
	}
	if !strings.Contains(got, `"type":"image"`) {
		t.Errorf("missing nested image type tag in: %s", got)
	}
	if !strings.Contains(got, `"type":"base64"`) {
		t.Errorf("missing source.type=base64 in: %s", got)
	}
}

// TestToAnthropic_ToolResultStringFallback — when ToolResultBlocks is
// empty (the 99% case), Content must serialise as the bare string
// the rest of the codebase has always produced. Regression guard
// against accidentally always emitting the multi-part shape, which
// would break every non-vision tool call.
func TestToAnthropic_ToolResultStringFallback(t *testing.T) {
	req := Request{
		Model: "x",
		Messages: []provider.Message{
			{
				Role: provider.RoleUser,
				Content: []provider.ContentBlock{
					{
						Type:       "tool_result",
						ToolUseID:  "toolu_x",
						ToolResult: "plain text result",
					},
				},
			},
		},
	}
	body := toAnthropic(req, "x", 100)
	tr := body.Messages[0].Content[0]
	if got, ok := tr.Content.(string); !ok || got != "plain text result" {
		t.Errorf("Content = %v (%T), want bare string \"plain text result\"", tr.Content, tr.Content)
	}
}
