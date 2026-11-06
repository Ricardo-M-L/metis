package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

type reviewLegacyRepository struct{ memory.Repository }

func TestSummaryMemoryNestedResumeAndAbsentAuthority(t *testing.T) {
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := memory.Passage{ID: "private", Content: "private fact", SourceSessionID: "origin"}
	if err := mm.Archival().Insert(p); err != nil {
		t.Fatal(err)
	}
	stored, err := mm.Search(memory.SearchOptions{SortBy: "recent"})
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored source: %v %v", stored, err)
	}
	p = stored[0]
	source := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: formatRecall([]memory.Passage{p}, ""), Synthetic: true}}}}
	first := llm.ContentBlock{Type: "text", Text: "paraphrased derived narrative", Synthetic: true, ProviderHint: summaryMemoryHints(source)}
	nested := []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_result", ToolResultBlocks: []llm.ContentBlock{first}}}}}
	second := llm.ContentBlock{Type: "text", Text: "twice derived narrative", Synthetic: true, ProviderHint: summaryMemoryHints(nested)}
	history := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "ship safe task"}}}, {Role: llm.RoleAssistant, Content: []llm.ContentBlock{second, {Type: "provider_state", Text: "old chain"}}}}
	raw, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	var restored []llm.Message
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		repo memory.Repository
	}{{"manager", mm}, {"absent", nil}, {"plugin", reviewLegacyRepository{mm}}} {
		t.Run(tc.name, func(t *testing.T) {
			l := &Loop{Memory: tc.repo, Messages: cloneMessages(restored)}
			if tc.name == "manager" {
				before := l.buildRequest(nil)
				if before.Messages[1].Content[0].Text != second.Text {
					t.Fatal("valid prefix rewritten")
				}
				if err := mm.DeleteSession("origin"); err != nil {
					t.Fatal(err)
				}
			}
			req := l.buildRequest(nil)
			foundNotice, foundTask := false, false
			for _, m := range req.Messages {
				for _, b := range m.Content {
					if b.Type == "provider_state" || strings.Contains(b.Text, "twice derived") {
						t.Fatal("derived source/chain survived")
					}
					foundNotice = foundNotice || strings.Contains(b.Text, "Earlier task details may be missing")
					foundTask = foundTask || b.Text == "ship safe task"
				}
			}
			if !foundNotice || !foundTask || l.memoryRevocationBefore == nil {
				t.Fatal("missing explicit recovery/task/durable replacement")
			}
		})
	}
}

func TestSummaryMemoryInvalidationPreservesToolPairing(t *testing.T) {
	block := llm.ContentBlock{Type: "tool_result", ToolUseID: "call-one", TraceCallID: "trace", ToolResult: "withdrawn derived body", ProviderHint: map[string]string{summaryMemoryHint: "malformed"}}
	got, removed := sanitizeDerivedMemory(block, "", nil, false)
	if !removed || got.Type != "tool_result" || got.ToolUseID != block.ToolUseID || got.TraceCallID != block.TraceCallID || strings.Contains(got.ToolResult, "derived body") {
		t.Fatalf("pair lost or revoked body retained: %+v", got)
	}
}

func TestSummaryMemoryCoreDependencyAppendAndWithdrawal(t *testing.T) {
	sections, _ := json.Marshal([]llm.SystemSection{{Name: "memory_index", Body: "core policy\nold private fact"}})
	hints := summaryMemoryHints([]llm.Message{{Role: llm.RoleSystem, Content: []llm.ContentBlock{{ProviderHint: map[string]string{dynamicCheckpointHint: "checkpoint", dynamicCheckpointHint + ".sections": string(sections)}}}}})
	if !validSummaryMemory(hints[summaryMemoryHint], "core policy\nold private fact\nnew fact", nil, true) {
		t.Fatal("append should preserve prefix")
	}
	if validSummaryMemory(hints[summaryMemoryHint], "core policy\nnew fact", nil, true) {
		t.Fatal("withdrawn core dependency accepted")
	}
	if strings.Contains(hints[summaryMemoryHint], "private fact") {
		t.Fatal("metadata retained withdrawn body")
	}
}

func TestReviewActualRepeatedCompactionPreservesDependencies(t *testing.T) {
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := memory.Passage{ID: "private", Content: "Nebula private endpoint is internal-nebula.example.", SourceSessionID: "source"}
	if err := mm.Archival().Insert(p); err != nil {
		t.Fatal(err)
	}
	stored, err := mm.Search(memory.SearchOptions{SortBy: "recent"})
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored source: %v %v", stored, err)
	}
	p = stored[0]
	provider := &reviewRecallSummaryProvider{fact: p.Content}
	cfg := DefaultCompactionConfig()
	cfg.RetainTokens = 200
	cfg.RetainMinMessages = 2
	cfg.RetainMinUserMessages = 1
	history := unifiedPipelineHistory()
	history[0].Content = append(history[0].Content, llm.ContentBlock{Type: "text", Text: formatRecall([]memory.Passage{p}, ""), Synthetic: true})
	l := &Loop{Memory: mm, Provider: provider, Messages: history, Compactor: NewCompactor(cfg, "review", 100000, provider)}
	for i := 0; i < 2; i++ {
		if i > 0 {
			l.Messages = append(l.Messages, unifiedPipelineHistory()...)
		}
		result, err := l.CompactNow(context.Background(), CompactOptions{Force: true})
		if err != nil || !result.Applied {
			t.Fatalf("compact %d applied=%v err=%v", i, result.Applied, err)
		}
	}
	if err := mm.DeleteSession("source"); err != nil {
		t.Fatal(err)
	}
	l.buildRequest(nil)
	if l.Compactor.LastSummary != "" {
		t.Fatal("revoked iterative seed retained")
	}
	for _, m := range l.History() {
		for _, b := range m.Content {
			if strings.Contains(b.Text, p.Content) {
				t.Fatal("twice compacted dependency lost")
			}
		}
	}
	// A subsequent summarizer must not receive the revoked seed.
	provider.requests = nil
	provider.fact = "safe replacement"
	l.Messages = append(l.Messages, unifiedPipelineHistory()...)
	if _, err := l.CompactNow(context.Background(), CompactOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) == 0 {
		t.Fatal("no third summarization request")
	}
	for _, req := range provider.requests {
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), p.Content) {
			t.Fatal("revoked fact reached later summarizer via iterative seed")
		}
	}
}
