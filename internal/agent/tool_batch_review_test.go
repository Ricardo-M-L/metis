package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

func TestReviewNativeImageBatch(t *testing.T) {
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
	b := llm.ContentBlock{Type: "tool_result", ToolUseID: "image", ToolResult: "image ready", ToolResultBlocks: []llm.ContentBlock{{Type: "text", Text: "image ready"}, {Type: "image", MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(buf.Bytes())}}}
	blocks := []llm.ContentBlock{b}
	l := &Loop{SpillDir: t.TempDir()}
	t.Logf("pngBytes=%d activeTokens=%d batchTokens=%d", buf.Len(), estimateActiveHistoryTokens([]llm.Message{{Role: llm.RoleUser, Content: blocks}}), batchTokens(blocks))
	if _, err := l.projectToolBatch(blocks); err != nil {
		t.Fatalf("native image below active input limit rejected: %v", err)
	}
	if batchTokens(blocks) != estimateActiveHistoryTokens([]llm.Message{{Role: llm.RoleUser, Content: blocks}}) {
		t.Fatal("batch/native active estimators diverged")
	}
	larger := b
	larger.ToolResultBlocks = append([]llm.ContentBlock(nil), b.ToolResultBlocks...)
	larger.ToolResultBlocks[1].Data = strings.Repeat(b.ToolResultBlocks[1].Data, 2)
	if batchTokens([]llm.ContentBlock{larger}) != batchTokens(blocks) {
		t.Fatal("base64 transport size was counted as context tokens")
	}
}

type reviewLargeErrorTool struct{ postHookErrorTool }

func (reviewLargeErrorTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return nil, errors.New(strings.Repeat("x", 6000))
}
func TestReviewErrorHookSurvivesBatch(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(reviewLargeErrorTool{})
	hooks := NewHookRegistry()
	hooks.Register(pubhook.PostToolUseContextHandler(func(context.Context, pubhook.Context, *pubhook.PostToolUse) *pubhook.ModifiedPostToolUse {
		return &pubhook.ModifiedPostToolUse{AdditionalContext: "SAFETY_SENTINEL"}
	}))
	l := &Loop{Registry: reg, Hooks: hooks, SpillDir: t.TempDir(), ToolResultTokenLimit: 800, ToolBatchTokenLimit: 1000}
	results, err := l.executeBatch(context.Background(), []llm.ContentBlock{{Type: "tool_use", ToolUseID: "error", ToolName: "PostHookError"}}, make(chan Event, 32), HookContext{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("prebudget_has_hook=%v trustedSuffix=%q", strings.Contains(results[0].ToolResult, "SAFETY_SENTINEL"), results[0].TrustedResultSuffix)
	out, err := l.projectToolBatch(results)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out[0].ToolResult, "SAFETY_SENTINEL") {
		t.Fatal("post-execute hook safety guidance removed from model projection")
	}
}
func TestReviewDeniedPlanEntryBatchBudget(t *testing.T) {
	calls := []llm.ContentBlock{{Type: "tool_use", ToolUseID: "enter", ToolName: "EnterPlanMode"}, {Type: "tool_use", ToolUseID: "write", ToolName: "Write"}}
	p := &planModeStreamProvider{streams: [][]llm.StreamEvent{toolBatchEvents(calls...)}}
	gate := permission.New(permission.ModeAcceptEdits)
	reg := tools.NewRegistry()
	reg.Register(planBatchTestTool{name: "EnterPlanMode", gate: gate, deny: true})
	reg.Register(planBatchTestTool{name: "Write", gate: gate})
	l := NewLoop(p, reg, gate, nil, "system", 5)
	l.SpillDir = t.TempDir()
	l.ToolResultTokenLimit = 50
	l.ToolBatchTokenLimit = 70
	l.AppendUser("plan then write")
	body := strings.Repeat("mandatory-user-steering ", 2000)
	l.Hooks.Register(pubhook.PreToolUseHandler(func(context.Context, pubhook.Context, *pubhook.PreToolUse) *pubhook.ModifiedPreToolUse {
		l.SteerInject(body)
		return nil
	}))
	var saved []llm.Message
	l.HistoryCheckpoint = func(history []llm.Message) error { saved = history; return nil }
	prefix := l.History()
	err := l.Run(context.Background(), make(chan Event, 100))
	var budgetErr *ToolBatchBudgetError
	if !errors.As(err, &budgetErr) {
		for _, m := range l.History() {
			if len(m.Content) > 0 && m.Content[0].Type == "tool_result" {
				t.Logf("batchTokens=%d limit=70", batchTokens(m.Content))
			}
		}
		t.Fatalf("denied-plan branch continued instead of applying mandatory budget: err=%v providerCalls=%d", err, p.calls)
	}
	if p.calls != 1 || continuationResultCount(saved) != 2 {
		t.Fatalf("denied-plan failure lost checkpoint pairing or continued: calls=%d results=%d", p.calls, continuationResultCount(saved))
	}
	if len(l.History()) <= len(prefix) || l.History()[0].Content[0].Text != prefix[0].Content[0].Text {
		t.Fatal("old prefix changed")
	}
	foundSteer := false
	for _, m := range saved {
		for _, b := range m.Content {
			if b.Type == "text" && strings.Contains(b.Text, strings.TrimSpace(body)) {
				foundSteer = true
			}
		}
	}
	if !foundSteer {
		t.Fatal("mandatory steering lost from checkpoint")
	}
}
