package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func memoryToolReceiptFixture(t *testing.T) (*memory.MemoryManager, llm.ContentBlock) {
	t.Helper()
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := memory.Passage{ID: "receipt-fact", Content: "Nebula uses Copper database.", Scope: "global", SourceSessionID: "receipt-source"}
	if err := mm.Archival().Insert(p); err != nil {
		t.Fatal(err)
	}
	meta := &tools.MemoryRecallResult{References: []tools.MemoryRecallReference{{ID: p.ID, Hash: recallPassageHash(p)}}, Passages: []tools.MemoryRecallPassage{{ID: p.ID, Content: p.Content, Scope: p.Scope, SourceSessionID: p.SourceSessionID}}}
	result := &tools.Result{Output: formatRecall([]memory.Passage{p}, ""), Meta: map[string]any{tools.MemoryRecallMetaKey: meta}}
	block := llm.ContentBlock{Type: "tool_result", ToolUseID: "memory-search-one", ToolResult: result.Output}
	attachMemoryToolProvenance(&block, "Memory", result, true)
	return mm, block
}

func assertNoMemoryUsageEvents(t *testing.T, root string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "usage", "events.jsonl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != "" {
		t.Fatal("no admitted request should record usage")
	}
}

func TestMemoryUsageReceiptWaitsForAdmittedRequestAndIsDurablyIdempotent(t *testing.T) {
	mm, block := memoryToolReceiptFixture(t)
	path := filepath.Join(mm.Root(), "archival", "passages.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertNoMemoryUsageEvents(t, mm.Root())
	l := &Loop{Memory: mm}
	req := llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{block}}}}
	for i := 0; i < 2; i++ {
		if err := l.commitRequestMemoryUsage(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	reloaded, err := memory.NewMemoryManager(mm.Root())
	if err != nil {
		t.Fatal(err)
	}
	l = &Loop{Memory: reloaded}
	if err := l.commitRequestMemoryUsage(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	ps, err := reloaded.Search(memory.SearchOptions{})
	if err != nil || len(ps) != 1 || ps[0].UseCount != 1 {
		t.Fatalf("usage after retry/restore = %+v, err=%v", ps, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("usage rewrote archival corpus")
	}
}

func TestMemoryUsageReceiptDoesNotCountBudgetReplacements(t *testing.T) {
	mm, block := memoryToolReceiptFixture(t)
	block.ToolResult = "[omitted by tool-result budget]"
	l := &Loop{Memory: mm}
	if err := l.commitRequestMemoryUsage(context.Background(), llm.Request{Messages: []llm.Message{{Content: []llm.ContentBlock{block}}}}); err != nil {
		t.Fatal(err)
	}
	assertNoMemoryUsageEvents(t, mm.Root())
}

func TestMemoryToolReceiptWithdrawsResultAndDerivedSummary(t *testing.T) {
	mm, block := memoryToolReceiptFixture(t)
	hints := summaryMemoryHints([]llm.Message{{Content: []llm.ContentBlock{block}}})
	l := &Loop{Memory: mm, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{block, {Type: "text", Text: "Derived historical fact", Synthetic: true, ProviderHint: hints}, {Type: "provider_state"}}}}}
	if err := mm.DeleteSession("receipt-source"); err != nil {
		t.Fatal(err)
	}
	req := l.buildRequest(nil)
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.ToolResult, "Copper") || b.Text == "Derived historical fact" || b.Type == "provider_state" {
				t.Fatal("withdrawn memory/continuation survived")
			}
		}
	}
	if err := l.commitRequestMemoryUsage(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	assertNoMemoryUsageEvents(t, mm.Root())
}

func TestMemoryToolMetadataCannotBeForgedByFenceOrOtherTool(t *testing.T) {
	_, block := memoryToolReceiptFixture(t)
	plain := llm.ContentBlock{Type: "tool_result", ToolResult: block.ToolResult}
	attachMemoryToolProvenance(&plain, "Read", &tools.Result{Output: block.ToolResult}, true)
	if len(plain.ProviderHint) != 0 {
		t.Fatal("untrusted fence acquired provenance")
	}
	var receipt memoryUsageReceipt
	if json.Unmarshal([]byte(block.ProviderHint[memoryUsageReceiptHint]), &receipt) != nil || receipt.EventID == "" {
		t.Fatal("missing host receipt")
	}
	for _, p := range receipt.Passages {
		if p.Content != "" {
			t.Fatal("receipt duplicated memory body")
		}
	}
}

func TestMemoryUsageReceiptInvalidRecoveryDoesNotRecordBeforeDispatch(t *testing.T) {
	t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", "invalid")
	mm, block := memoryToolReceiptFixture(t)
	provider := &memoryRepositoryE2EProvider{}
	l := NewLoop(provider, tools.NewRegistry(), nil, NewHookRegistry(), "test", 1)
	l.Memory = mm
	l.DistillEvery = 0
	l.Messages = []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Recall Nebula"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: block.ToolUseID, ToolName: "Memory", ToolInput: map[string]any{"action": "search", "query": "Nebula"}}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{block}},
	}
	if err := l.Run(context.Background(), make(chan Event, 64)); err == nil {
		t.Fatal("invalid recovery policy accepted")
	}
	provider.mu.Lock()
	calls := len(provider.streamRequests)
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatal("provider called despite invalid local policy")
	}
	assertNoMemoryUsageEvents(t, mm.Root())
}

func TestMemoryToolCoreReadWithdrawsResultWithoutArchiveUsage(t *testing.T) {
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const fact = "The user prefers Nebula release checks."
	if _, err := mm.AddCoreBlock("user", fact); err != nil {
		t.Fatal(err)
	}
	result := &tools.Result{Output: fact, Meta: map[string]any{tools.MemoryRecallMetaKey: &tools.MemoryRecallResult{ContextLineHashes: []string{memoryLineHash(fact)}}}}
	block := llm.ContentBlock{Type: "tool_result", ToolUseID: "core-read", ToolResult: fact}
	attachMemoryToolProvenance(&block, "Memory", result, true)
	if block.ProviderHint[summaryMemoryHint] == "" || block.ProviderHint[memoryUsageReceiptHint] != "" {
		t.Fatal("core read needs provenance, not archival usage")
	}
	l := &Loop{Memory: mm, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{block}}}}
	if _, err := mm.RemoveCoreBlock("user", fact); err != nil {
		t.Fatal(err)
	}
	req := l.buildRequest(nil)
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.ToolResult, fact) {
				t.Fatal("withdrawn core read survived")
			}
		}
	}
	if err := l.commitRequestMemoryUsage(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	assertNoMemoryUsageEvents(t, mm.Root())
}
