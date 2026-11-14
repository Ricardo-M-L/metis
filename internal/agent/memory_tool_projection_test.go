package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type memoryProjectionTool struct{ result *tools.Result }

func (memoryProjectionTool) Name() string                { return "Memory" }
func (memoryProjectionTool) Description() string         { return "memory projection fixture" }
func (memoryProjectionTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (memoryProjectionTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (memoryProjectionTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (memoryProjectionTool) IsEnabled() bool         { return true }
func (memoryProjectionTool) MaxResultSizeChars() int { return tools.ResultSizeUnlimited }
func (t memoryProjectionTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return t.result, nil
}

func TestMemoryToolProjectionDoesNotCreateUnrevocableSpill(t *testing.T) {
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	large := memory.Passage{ID: "too-large", Content: strings.Repeat("unrelated material ", 4000), Scope: "global", SourceSessionID: "large-source"}
	small := memory.Passage{ID: "visible", Content: "Nebula uses Copper database.", Scope: "global", SourceSessionID: "visible-source"}
	meta := &tools.MemoryRecallResult{}
	var body strings.Builder
	for _, p := range []memory.Passage{large, small} {
		if err := mm.Archival().Insert(p); err != nil {
			t.Fatal(err)
		}
		body.WriteString(formatRecall([]memory.Passage{p}, ""))
		meta.References = append(meta.References, tools.MemoryRecallReference{ID: p.ID, Hash: recallPassageHash(p)})
		meta.Passages = append(meta.Passages, tools.MemoryRecallPassage{ID: p.ID, Content: p.Content, Scope: p.Scope, SourceSessionID: p.SourceSessionID})
	}
	reg := tools.NewRegistry()
	reg.Register(memoryProjectionTool{&tools.Result{Output: body.String(), Meta: map[string]any{tools.MemoryRecallMetaKey: meta}}})
	spillDir := t.TempDir()
	l := &Loop{Memory: mm, Registry: reg, SpillDir: spillDir, ToolResultTokenLimit: 1200, ToolBatchTokenLimit: 1400}
	blocks, err := l.executeBatch(context.Background(), []llm.ContentBlock{{Type: "tool_use", ToolUseID: "large-memory", ToolName: "Memory"}}, make(chan Event, 16), HookContext{})
	if err != nil || len(blocks) != 1 {
		t.Fatalf("dispatch=%d err=%v", len(blocks), err)
	}
	if blocks[0].ProviderHint[memoryToolObservationHint] != "1" {
		t.Fatal("missing typed host provenance")
	}
	projected, err := l.projectToolBatch(blocks)
	if err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(spillDir)
	if err != nil || len(files) != 0 {
		t.Fatalf("Memory created ordinary Read spill: %d err=%v", len(files), err)
	}
	if !strings.Contains(projected[0].ToolResult, small.Content) || strings.Contains(projected[0].ToolResult, large.Content[:100]) {
		t.Fatal("projection did not retain only fitting source units")
	}
	var deps summaryMemoryDependencies
	if json.Unmarshal([]byte(projected[0].ProviderHint[summaryMemoryHint]), &deps) != nil || len(deps.References) != 1 || deps.References[0].ID != small.ID {
		t.Fatal("projection retained omitted source dependencies")
	}
	encoded, err := json.Marshal([]llm.Message{{Role: llm.RoleUser, Content: projected}})
	if err != nil {
		t.Fatal(err)
	}
	var history []llm.Message
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	l = &Loop{Memory: mm, Messages: history}
	if err := l.commitRequestMemoryUsage(context.Background(), l.buildRequest(nil)); err != nil {
		t.Fatal(err)
	}
	ps, err := mm.Search(memory.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if (p.ID == small.ID && p.UseCount != 1) || (p.ID == large.ID && p.UseCount != 0) {
			t.Fatalf("wrong admitted usage: %s=%d", p.ID, p.UseCount)
		}
	}
	if err := mm.DeleteSession(large.SourceSessionID); err != nil {
		t.Fatal(err)
	}
	if req := l.buildRequest(nil); !strings.Contains(req.Messages[0].Content[0].ToolResult, small.Content) {
		t.Fatal("omitted source revoked unrelated retained result")
	}
	if err := mm.DeleteSession(small.SourceSessionID); err != nil {
		t.Fatal(err)
	}
	req := l.buildRequest(nil)
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.ToolResult, small.Content) {
				t.Fatal("revoked projected fact survived restore")
			}
		}
	}
}

func TestMemoryCoreProjectionOnlyRetainsCompleteAuthorizedLines(t *testing.T) {
	long := strings.Repeat("oversized ", 3000)
	short := "preference: Chinese replies"
	deps := summaryMemoryDependencies{ContextLines: []string{memoryResultHash(long), memoryResultHash(short)}}
	raw, _ := json.Marshal(deps)
	b := llm.ContentBlock{Type: "tool_result", ToolResult: long + "\n" + short, Synthetic: true, ProviderHint: map[string]string{summaryMemoryHint: string(raw), memoryToolObservationHint: "1"}}
	l := &Loop{ToolResultTokenLimit: 600, ToolBatchTokenLimit: 800}
	out, err := l.projectToolBatch([]llm.ContentBlock{b})
	if err != nil {
		t.Fatal(err)
	}
	var actual summaryMemoryDependencies
	if json.Unmarshal([]byte(out[0].ProviderHint[summaryMemoryHint]), &actual) != nil || len(actual.ContextLines) != 1 || actual.ContextLines[0] != memoryResultHash(short) {
		t.Fatal("Core projection did not track its complete retained line")
	}
	if !strings.Contains(out[0].ToolResult, short) || strings.Contains(out[0].ToolResult, long[:100]) {
		t.Fatal("Core result retained an untracked partial line")
	}
}

func TestMemoryObservationCannotBecomeUntrackedHistoricalSpill(t *testing.T) {
	body := strings.Repeat("memory source evidence ", 400)
	block := llm.ContentBlock{Type: "tool_result", ToolUseID: "saved-memory", ToolResult: body, Synthetic: true, ProviderHint: map[string]string{memoryToolObservationHint: "1"}}
	messages := []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: block.ToolUseID, ToolName: "Memory"}}}, {Role: llm.RoleUser, Content: []llm.ContentBlock{block}}}
	c := NewCompactor(DefaultCompactionConfig(), "test", 1000, &fakeSummarizer{})
	c.ProtectFirst, c.ProtectLast, c.KeepRecentToolResults = 0, 0, 0
	c.ProtectedTools = nil // Typed provenance must survive even custom compactor settings.
	c.MicrocompactDir = t.TempDir()
	c.MicrocompactMinChars, c.SnipMaxToolResultChars = 10, 10
	for name, project := range map[string]func([]llm.Message) []llm.Message{"microcompact": c.Microcompact, "snip": c.Snip, "snip_all": c.SnipAll, "post_compact": c.EnforcePostCompactBudget} {
		t.Run(name, func(t *testing.T) {
			if got := project(messages); got[1].Content[0].ToolResult != body {
				t.Fatal("rewrote a revocable memory result outside first-ingestion admission")
			}
		})
	}
	if files, err := os.ReadDir(c.MicrocompactDir); err != nil || len(files) != 0 {
		t.Fatalf("untracked historical spill files=%d err=%v", len(files), err)
	}
}
