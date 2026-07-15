package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/pkg/provider"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

// stubVisionProvider implements provider.Provider AND the optional
// provider.VisionSupporter so tests can simulate a vision-capable
// model without touching real network code. visionOK chooses which
// branch of the dispatch image-gate fires.
type stubVisionProvider struct{ visionOK bool }

func (s stubVisionProvider) Name() string          { return "stub" }
func (s stubVisionProvider) ModelID() string       { return "stub-model" }
func (s stubVisionProvider) MaxContextTokens() int { return 100_000 }
func (s stubVisionProvider) SupportsVision() bool  { return s.visionOK }
func (s stubVisionProvider) Complete(context.Context, provider.Request) (*provider.Response, error) {
	return nil, io.EOF
}
func (s stubVisionProvider) Stream(context.Context, provider.Request) (provider.StreamReader, error) {
	return nil, io.EOF
}

// visionTool — a stub that returns one inline image so we can verify
// the dispatch layer fans Result.Images into ContentBlock.ToolResultBlocks.
// Mirrors the shape ViewImage produces, minus the disk read.
type visionTool struct{}

func (visionTool) Name() string                                 { return "VisionStub" }
func (visionTool) Description() string                          { return "test vision stub" }
func (visionTool) InputSchema() map[string]any                  { return map[string]any{"type": "object"} }
func (visionTool) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (visionTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (visionTool) IsEnabled() bool { return true }
func (visionTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{
		Output: "VisionStub: 1 image attached",
		Images: []pubtool.ImageAttachment{
			{MediaType: "image/png", Data: "iVBORw0KGgo="},
		},
	}, nil
}

// TestDispatch_PromotesImagesToToolResultBlocks — when a tool returns
// Result.Images AND the configured provider supports vision, dispatch
// MUST produce a Type="tool_result" ContentBlock with
// ToolResultBlocks=[{text}, {image}]. Without this the provider
// adapter can't tell the result is multi-part and falls back to
// text-only serialisation — model never sees the image.
func TestDispatch_PromotesImagesToToolResultBlocks(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(visionTool{})

	loop := &Loop{Registry: reg, Provider: stubVisionProvider{visionOK: true}}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "tu_vision", ToolName: "VisionStub"},
	}
	out := make(chan Event, 16)
	results, err := loop.executeBatch(context.Background(), uses, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	r := results[0]
	if r.Type != "tool_result" {
		t.Fatalf("result.Type = %q, want tool_result", r.Type)
	}
	if r.ToolUseID != "tu_vision" {
		t.Errorf("ToolUseID = %q, want tu_vision", r.ToolUseID)
	}
	if r.ToolResult != "VisionStub: 1 image attached" {
		t.Errorf("ToolResult fallback string lost; got %q", r.ToolResult)
	}
	if len(r.ToolResultBlocks) != 2 {
		t.Fatalf("ToolResultBlocks count = %d, want 2 (text+image)", len(r.ToolResultBlocks))
	}
	if r.ToolResultBlocks[0].Type != "text" {
		t.Errorf("ToolResultBlocks[0].Type = %q, want text", r.ToolResultBlocks[0].Type)
	}
	if r.ToolResultBlocks[1].Type != "image" {
		t.Errorf("ToolResultBlocks[1].Type = %q, want image", r.ToolResultBlocks[1].Type)
	}
	if r.ToolResultBlocks[1].MediaType != "image/png" {
		t.Errorf("image MediaType = %q, want image/png", r.ToolResultBlocks[1].MediaType)
	}
	if r.ToolResultBlocks[1].Data != "iVBORw0KGgo=" {
		t.Errorf("image Data lost; got %q", r.ToolResultBlocks[1].Data)
	}
}

// TestDispatch_NonVisionProviderStripsImageAndExplains — when the
// provider isn't vision-capable (deepseek-v4-pro on
// /chat/completions is the canonical case as of 2026-05-20), the
// dispatch layer MUST drop the image fan-out and append a system-
// reminder to the textual tool_result explaining the drop. Two
// invariants matter: (1) ToolResultBlocks stays empty so the
// adapter doesn't emit image_url and trigger a 400 from the
// upstream API; (2) the reminder mentions "vision" so the model
// understands why the image isn't visible to it.
func TestDispatch_NonVisionProviderStripsImageAndExplains(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(visionTool{})

	loop := &Loop{Registry: reg, Provider: stubVisionProvider{visionOK: false}}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "tu_vision_strip", ToolName: "VisionStub"},
	}
	out := make(chan Event, 16)
	results, _ := loop.executeBatch(context.Background(), uses, out, HookContext{})
	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	r := results[0]
	if len(r.ToolResultBlocks) != 0 {
		t.Errorf("non-vision provider: ToolResultBlocks must stay empty (else adapter emits image_url and API 400s); got %d", len(r.ToolResultBlocks))
	}
	body := r.ToolResult
	// The textual fallback should still be present, plus a system-
	// reminder mentioning "vision" so the model knows why the
	// image is missing.
	if body == "" {
		t.Error("ToolResult body is empty; expected text fallback + system-reminder")
	}
	if !strings.Contains(body, "vision") {
		t.Errorf("ToolResult body missing 'vision' hint; got %q", body)
	}
	if !strings.Contains(body, "dropped") {
		t.Errorf("ToolResult body missing 'dropped' marker; got %q", body)
	}
}

// textTool — a normal tool that returns only Output (no Images), the
// 99% path. Used to pin the fallback shape: ToolResultBlocks MUST be
// empty so the adapter takes the bare-string code path.
type textTool struct{}

func (textTool) Name() string                                 { return "TextStub" }
func (textTool) Description() string                          { return "test text stub" }
func (textTool) InputSchema() map[string]any                  { return map[string]any{"type": "object"} }
func (textTool) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (textTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (textTool) IsEnabled() bool { return true }
func (textTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "ok"}, nil
}

// TestDispatch_TextResultLeavesToolResultBlocksEmpty — regression
// guard: only Images-bearing tools should trigger the multi-part
// promotion. A text-only tool MUST leave ToolResultBlocks=nil so the
// anthropic adapter falls back to the bare-string Content shape every
// existing tool depends on.
func TestDispatch_TextResultLeavesToolResultBlocksEmpty(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(textTool{})

	loop := &Loop{Registry: reg}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "tu_text", ToolName: "TextStub"},
	}
	out := make(chan Event, 16)
	results, _ := loop.executeBatch(context.Background(), uses, out, HookContext{})
	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	if len(results[0].ToolResultBlocks) != 0 {
		t.Errorf("text-only result should not populate ToolResultBlocks; got %d entries", len(results[0].ToolResultBlocks))
	}
}
