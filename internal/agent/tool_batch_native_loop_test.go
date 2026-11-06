package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

type batchNativeProvider struct {
	planModeStreamProvider
	requests []llm.Request
}

func (p *batchNativeProvider) SupportsVision() bool { return true }
func (p *batchNativeProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	p.requests = append(p.requests, req)
	return p.planModeStreamProvider.Stream(ctx, req)
}

type batchScreenshotTool struct {
	visionTool
	data string
}

func (batchScreenshotTool) Name() string { return "Screenshot" }
func (t batchScreenshotTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "screenshot ready", Images: []pubtool.ImageAttachment{{MediaType: "image/png", Data: t.data}}}, nil
}

// Model the ViewImage attachment contract here; the builtin has separate file
// ingestion tests and cannot be imported by package agent (it imports agent).
type batchViewImageTool struct{ visionTool }

func (batchViewImageTool) Name() string { return "ViewImage" }
func (batchViewImageTool) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	raw, err := os.ReadFile(in["path"].(string))
	if err != nil {
		return nil, err
	}
	return &tools.Result{Output: "ViewImage ready", Images: []pubtool.ImageAttachment{{MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(raw)}}}, nil
}

func batchNativePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	x := uint32(17)
	for i := range img.Pix {
		x = x*1664525 + 1013904223
		img.Pix[i] = byte(x >> 24)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func TestToolBatchNativeImagesRealLoop(t *testing.T) {
	raw := batchNativePNG(t)
	path := filepath.Join(t.TempDir(), "native.png")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	calls := []llm.ContentBlock{{Type: "tool_use", ToolUseID: "shot", ToolName: "Screenshot"}, {Type: "tool_use", ToolUseID: "view", ToolName: "ViewImage", ToolInput: map[string]any{"path": path}}}
	events := toolBatchEvents(calls...)
	input, _ := json.Marshal(calls[1].ToolInput)
	events[4].InputDelta = string(input)
	p := &batchNativeProvider{planModeStreamProvider: planModeStreamProvider{streams: [][]llm.StreamEvent{events}}}
	reg := tools.NewRegistry()
	reg.Register(batchScreenshotTool{data: base64.StdEncoding.EncodeToString(raw)})
	reg.Register(batchViewImageTool{})
	l := NewLoop(p, reg, nil, nil, "stable system", 5)
	l.SpillDir = t.TempDir()
	l.AppendUser("inspect both images")
	prefix := l.History()
	var saved []llm.Message
	l.HistoryCheckpoint = func(h []llm.Message) error { saved = h; return nil }
	if err := l.Run(context.Background(), make(chan Event, 100)); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) < 2 {
		t.Fatalf("image continuation missing: requests=%d", len(p.requests))
	}
	if !reflect.DeepEqual(l.History()[:len(prefix)], prefix) {
		t.Fatal("old prefix changed")
	}
	if continuationResultCount(saved) != 2 {
		t.Fatal("checkpoint lost image pairing")
	}
	var batch []llm.ContentBlock
	for _, m := range p.requests[1].Messages {
		if len(m.Content) > 0 && m.Content[0].Type == "tool_result" {
			batch = m.Content
		}
	}
	if len(batch) != 2 {
		t.Fatalf("image results=%d", len(batch))
	}
	for i, b := range batch {
		if b.ToolUseID != calls[i].ToolUseID || len(b.ToolResultBlocks) != 2 || b.ToolResultBlocks[1].Data != base64.StdEncoding.EncodeToString(raw) {
			t.Fatalf("image %d lost or changed: id=%s body=%s parts=%d", i, b.ToolUseID, b.ToolResult, len(b.ToolResultBlocks))
		}
	}
	if batchTokens(batch) != estimateActiveHistoryTokens([]llm.Message{{Role: llm.RoleUser, Content: batch}}) {
		t.Fatal("native estimates disagree")
	}
	// Aggregate image+text pressure must still fail explicitly, with complete pairs.
	l.ToolBatchTokenLimit = batchTokens(batch) - 1
	batch = append(batch, llm.ContentBlock{Type: "text", Text: strings.Repeat("mandatory steering ", 1000)})
	before := len(l.History())
	var budgetErr *ToolBatchBudgetError
	if err := l.commitToolBatch(batch); !errors.As(err, &budgetErr) {
		t.Fatalf("mandatory overflow: %v", err)
	}
	if !reflect.DeepEqual(l.History()[before].Content, batch) || !reflect.DeepEqual(saved[len(saved)-1].Content, batch) {
		t.Fatal("mandatory failure lost full paired batch/checkpoint")
	}
}

func TestToolBatchGoErrorHookRealLoop(t *testing.T) {
	call := llm.ContentBlock{Type: "tool_use", ToolUseID: "error", ToolName: "PostHookError"}
	for _, mandatory := range []bool{false, true} {
		t.Run(map[bool]string{false: "spill", true: "mandatory-overflow"}[mandatory], func(t *testing.T) {
			p := &batchNativeProvider{planModeStreamProvider: planModeStreamProvider{streams: [][]llm.StreamEvent{toolBatchEvents(call)}}}
			reg := tools.NewRegistry()
			reg.Register(reviewLargeErrorTool{})
			l := NewLoop(p, reg, nil, nil, "system", 5)
			l.SpillDir = t.TempDir()
			l.ToolResultTokenLimit = 800
			l.ToolBatchTokenLimit = 1000
			l.AppendUser("run failing tool")
			suffix := "SAFETY_SENTINEL"
			if mandatory {
				suffix = strings.Repeat(suffix, 1000)
			}
			l.Hooks.Register(pubhook.PostToolUseContextHandler(func(context.Context, pubhook.Context, *pubhook.PostToolUse) *pubhook.ModifiedPostToolUse {
				return &pubhook.ModifiedPostToolUse{AdditionalContext: suffix}
			}))
			var saved []llm.Message
			l.HistoryCheckpoint = func(h []llm.Message) error { saved = h; return nil }
			err := l.Run(context.Background(), make(chan Event, 100))
			var budgetErr *ToolBatchBudgetError
			if mandatory {
				if !errors.As(err, &budgetErr) || len(p.requests) != 1 {
					t.Fatalf("mandatory hook must stop before continuation: %v requests=%d", err, len(p.requests))
				}
			} else if err != nil || len(p.requests) < 2 {
				t.Fatalf("spill continuation: %v requests=%d", err, len(p.requests))
			}
			if continuationResultCount(saved) != 1 {
				t.Fatal("error pairing not checkpointed")
			}
			for _, m := range saved {
				for _, b := range m.Content {
					if b.Type == "tool_result" {
						if !b.IsError || b.ToolUseID != "error" || !strings.Contains(b.ToolResult, suffix) {
							t.Fatal("error hook lost")
						}
						if !mandatory && batchTokens(m.Content) > 1000 {
							t.Fatal("error exceeds batch budget")
						}
					}
				}
			}
		})
	}
}
