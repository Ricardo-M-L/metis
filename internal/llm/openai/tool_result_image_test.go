package openai

import (
	"testing"
)

// TestToOpenAI_ToolResultImageFanOut — a tool_result block with
// ToolResultBlocks=[{text}, {image}] (the shape ViewImage produces)
// must split: the text rides as the role="tool" message content,
// and the image gets folded into a synthetic role="user" message
// that follows the tool reply. OpenAI's tool role accepts ONLY a
// string Content, so this fan-out is the only way DeepSeek / Kimi
// / GLM (every openai-compat custom provider) can actually receive
// vision payloads from tools.
func TestToOpenAI_ToolResultImageFanOut(t *testing.T) {
	req := Request{
		Messages: []Message{{
			Role: RoleUser,
			Content: []ContentBlock{
				{
					Type:       "tool_result",
					ToolUseID:  "toolu_vision",
					ToolResult: "ViewImage: /tmp/x.png (123 bytes, image/png)",
					ToolResultBlocks: []ContentBlock{
						{Type: "text", Text: "ViewImage: /tmp/x.png (123 bytes, image/png)"},
						{
							Type:      "image",
							MediaType: "image/png",
							Data:      "iVBORw0KGgoAAAANSUhEUg==",
						},
					},
				},
			},
		}},
	}
	out := toOpenAI(req, "deepseek-v4-pro", 1024)
	if len(out.Messages) != 2 {
		t.Fatalf("expected 2 messages (tool + synthetic user); got %d", len(out.Messages))
	}
	if out.Messages[0].Role != "tool" {
		t.Errorf("Messages[0].Role = %q, want tool", out.Messages[0].Role)
	}
	if got, _ := out.Messages[0].Content.(string); got != "ViewImage: /tmp/x.png (123 bytes, image/png)" {
		t.Errorf("tool message Content = %v (%T); want plain text from ToolResultBlocks", out.Messages[0].Content, out.Messages[0].Content)
	}
	if out.Messages[1].Role != "user" {
		t.Errorf("Messages[1].Role = %q, want user (synthetic carrier for the image)", out.Messages[1].Role)
	}
	parts, ok := out.Messages[1].Content.([]oaiContentPart)
	if !ok {
		t.Fatalf("user message Content = %T; want []oaiContentPart with image_url parts", out.Messages[1].Content)
	}
	if len(parts) != 1 {
		t.Fatalf("expected 1 part (image_url); got %d", len(parts))
	}
	if parts[0].Type != "image_url" || parts[0].ImageURL == nil {
		t.Fatalf("part[0] = %+v; want image_url with non-nil ImageURL", parts[0])
	}
	wantURL := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="
	if parts[0].ImageURL.URL != wantURL {
		t.Errorf("image data URI = %q; want %q", parts[0].ImageURL.URL, wantURL)
	}
}

// TestToOpenAI_ToolResultStringFallback — when ToolResultBlocks is
// empty (99% of tools), the tool message Content must still be the
// bare ToolResult string and NO synthetic user message gets emitted.
// Regression guard against accidentally always running the fan-out.
func TestToOpenAI_ToolResultStringFallback(t *testing.T) {
	req := Request{
		Messages: []Message{{
			Role: RoleUser,
			Content: []ContentBlock{
				{
					Type:       "tool_result",
					ToolUseID:  "toolu_plain",
					ToolResult: "plain text result",
				},
			},
		}},
	}
	out := toOpenAI(req, "deepseek-v4-pro", 1024)
	if len(out.Messages) != 1 {
		t.Fatalf("expected 1 message (tool only); got %d", len(out.Messages))
	}
	if out.Messages[0].Role != "tool" {
		t.Errorf("Role = %q, want tool", out.Messages[0].Role)
	}
	if got, _ := out.Messages[0].Content.(string); got != "plain text result" {
		t.Errorf("Content = %v (%T); want bare string", out.Messages[0].Content, out.Messages[0].Content)
	}
}

// TestToOpenAI_ToolResultImageWithSubsequentUserText — mixed turn:
// the user replied with text AND the prior tool returned an image.
// Both the image (from tool fan-out) and the text should land in
// the same synthetic user message, image AFTER text so the model
// reads the question first then sees the visual.
func TestToOpenAI_ToolResultImageWithSubsequentUserText(t *testing.T) {
	req := Request{
		Messages: []Message{{
			Role: RoleUser,
			Content: []ContentBlock{
				{
					Type:       "tool_result",
					ToolUseID:  "toolu_x",
					ToolResult: "img loaded",
					ToolResultBlocks: []ContentBlock{
						{Type: "text", Text: "img loaded"},
						{Type: "image", MediaType: "image/jpeg", Data: "/9j/AA=="},
					},
				},
				{Type: "text", Text: "what's in the picture?"},
			},
		}},
	}
	out := toOpenAI(req, "deepseek-v4-pro", 1024)
	if len(out.Messages) != 2 {
		t.Fatalf("expected 2 messages (tool + user); got %d", len(out.Messages))
	}
	parts, ok := out.Messages[1].Content.([]oaiContentPart)
	if !ok {
		t.Fatalf("user Content = %T; want parts array", out.Messages[1].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts (text + image); got %d", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "what's in the picture?" {
		t.Errorf("part[0] = %+v; want user text first", parts[0])
	}
	if parts[1].Type != "image_url" {
		t.Errorf("part[1] = %+v; want image_url second", parts[1])
	}
}
