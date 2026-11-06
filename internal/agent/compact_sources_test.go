package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

// Promoted from /tmp/metis-memory-review2-20261106/tail_dependencies_test.go;
// the provider request, not the fitter helper, establishes the actual input.
func TestReviewTailOnlyMemoryDoesNotInvalidateUnrelatedSummary(t *testing.T) {
	testSummaryActualSources(t, "tail", 0, false)
}
func TestSummaryActualSourcesParticipating(t *testing.T) {
	testSummaryActualSources(t, "source", 0, true)
}
func TestSummaryActualSourcesFullyExcluded(t *testing.T) {
	testSummaryActualSources(t, "excluded", 800, false)
}
func TestSummaryActualSourcesPartiallyRetained(t *testing.T) {
	testSummaryActualSources(t, "partial", 1800, true)
}

func testSummaryActualSources(t *testing.T, kind string, cap int, dependent bool) {
	t.Helper()
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := memory.Passage{ID: "source", Content: "SOURCE_START " + strings.Repeat("private evidence ", 800) + " SOURCE_END", SourceSessionID: "origin"}
	if kind != "partial" {
		p.Content = "Unique private source evidence."
	}
	if err := mm.Archival().Insert(p); err != nil {
		t.Fatal(err)
	}
	stored, err := mm.Search(memory.SearchOptions{SortBy: "recent"})
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored: %v %v", stored, err)
	}
	p = stored[0]
	history := unifiedPipelineHistory()
	index := 0
	if kind == "tail" {
		index = len(history) - 2
	}
	if kind == "excluded" {
		index = 10
	}
	history[index].Content = append(history[index].Content, llm.ContentBlock{Type: "text", Text: formatRecall([]memory.Passage{p}, ""), Synthetic: true})
	provider := &reviewRecallSummaryProvider{fact: "Public task validation finished."}
	cfg := DefaultCompactionConfig()
	cfg.RetainTokens = 200
	cfg.RetainMinMessages = 2
	cfg.RetainMinUserMessages = 1
	cfg.MaxSummarizeInputTokens = cap
	l := &Loop{Memory: mm, Provider: provider, Messages: history, Compactor: NewCompactor(cfg, "review", 100000, provider)}
	result, err := l.CompactNow(context.Background(), CompactOptions{Force: true})
	if err != nil || !result.Applied {
		t.Fatalf("applied=%v err=%v", result.Applied, err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("summary calls=%d", len(provider.requests))
	}
	payload := provider.requests[0].Messages[0].Content[0].Text
	marker := p.Content
	if kind == "partial" {
		marker = "SOURCE_START"
	}
	if strings.Contains(payload, marker) != dependent {
		t.Fatalf("fixture actual payload participation=%v want=%v; payload=%s", strings.Contains(payload, marker), dependent, payload)
	}
	if kind == "partial" && strings.Contains(payload, p.Content) {
		t.Fatal("fixture was not partially fitted")
	}
	if err := mm.DeleteSession("origin"); err != nil {
		t.Fatal(err)
	}
	req := l.buildRequest(nil)
	present := false
	for _, m := range req.Messages {
		for _, b := range m.Content {
			present = present || strings.Contains(b.Text, provider.fact)
		}
	}
	if present == dependent {
		t.Fatalf("summary survived=%v; dependent=%v", present, dependent)
	}
}

func TestSummaryActualSourcesDirectReuse(t *testing.T) {
	for _, iterative := range []bool{false, true} {
		t.Run(istr(boolInt(iterative)), func(t *testing.T) {
			p := &reviewRecallSummaryProvider{fact: "unused"}
			cfg := DefaultCompactionConfig()
			cfg.IterativeSummary = iterative
			cfg.RetainTokens = 64
			cfg.RetainMinMessages = 1
			hints := summaryMemoryHints([]llm.Message{{Content: []llm.ContentBlock{{Type: "text", Synthetic: true, Text: formatRecall([]memory.Passage{{ID: "old", Content: "old fact"}}, "")}}}})
			history := []llm.Message{msg(llm.RoleUser, compactionCheckpointRequestText), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Synthetic: true, Text: "[Earlier conversation summarized: old derived summary]", ProviderHint: hints}}}, msg(llm.RoleUser, strings.Repeat("recent task ", 100))}
			c := NewCompactor(cfg, "review", 100000, p)
			out, err := c.Compact(context.Background(), history)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.requests) != 0 {
				t.Fatal("direct reuse unexpectedly invoked model")
			}
			found := false
			for _, m := range out {
				for _, b := range m.Content {
					if strings.Contains(b.Text, "old derived summary") {
						found = true
						if b.ProviderHint[summaryMemoryHint] != hints[summaryMemoryHint] {
							t.Fatal("direct reuse lost old summary dependencies")
						}
					}
				}
			}
			if !found {
				t.Fatal("old summary not reused")
			}
		})
	}
}
func TestSummaryActualSourcesSeedUsage(t *testing.T) {
	for _, iterative := range []bool{false, true} {
		t.Run(istr(boolInt(iterative)), func(t *testing.T) {
			p := &reviewRecallSummaryProvider{fact: "new public summary"}
			cfg := DefaultCompactionConfig()
			cfg.IterativeSummary = iterative
			cfg.RetainTokens = 200
			cfg.RetainMinMessages = 2
			c := NewCompactor(cfg, "review", 100000, p)
			c.LastSummary = "OLD_SEED"
			c.lastSummaryMemory = summaryMemoryHints([]llm.Message{{Content: []llm.ContentBlock{{Type: "text", Synthetic: true, Text: formatRecall([]memory.Passage{{ID: "seed", Content: "seed fact"}}, "")}}}})
			out, err := c.Compact(context.Background(), unifiedPipelineHistory())
			if err != nil {
				t.Fatal(err)
			}
			if len(p.requests) != 1 || strings.Contains(p.requests[0].Messages[0].Content[0].Text, "OLD_SEED") != iterative {
				t.Fatal("seed payload differs from configured use")
			}
			for _, m := range out {
				for _, b := range m.Content {
					if strings.Contains(b.Text, p.fact) && (b.ProviderHint[summaryMemoryHint] != "") != iterative {
						t.Fatal("unused seed inherited or used seed lost")
					}
				}
			}
		})
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
