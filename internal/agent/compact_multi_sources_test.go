package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"strings"
	"testing"
)

func TestSummarySourceRepeatedBodiesUsePositions(t *testing.T) {
	s := "identical source body"
	before := s + strings.Repeat("x", 100) + s
	sources := []summarySourceSpan{{0, len(s), llm.ContentBlock{Text: "head"}}, {len(s) + 20, len(s) + 40, llm.ContentBlock{Text: "middle"}}, {len(before) - len(s), len(before), llm.ContentBlock{Text: "tail"}}}
	after := compactRuneWindow(before, len(s), len(s), "summary message")
	spans := projectSummarySpans(before, after, sources)
	ids := map[string]bool{}
	for _, span := range spans {
		ids[span.source.Text] = true
	}
	if len(ids) != 2 || !ids["head"] || !ids["tail"] {
		t.Fatalf("repeated bytes misattributed: %v", ids)
	}
}

func TestSummaryRecallNumberedBodyAndRedaction(t *testing.T) {
	p := memory.Passage{ID: "numbered", Content: "api_key: sk-abcdefghijklmnopqrstuvwxyz123456\n[2] quoted numbered source line\nlast fact"}
	b := llm.ContentBlock{Type: "text", Synthetic: true, Text: formatRecall([]memory.Passage{p}, "")}
	provider := &reviewRecallSummaryProvider{fact: "summary"}
	cfg := DefaultCompactionConfig()
	cfg.RedactSecrets = true
	c := NewCompactor(cfg, "review", 100000, provider)
	var hints map[string]string
	if _, err := c.summarizeWithSources(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{b}}}, "", "", nil, &hints); err != nil {
		t.Fatal(err)
	}
	payload := provider.requests[0].Messages[0].Content[0].Text
	if !strings.Contains(payload, "[2] quoted numbered source line") || strings.Contains(payload, "sk-abcdefghijklmnopqrstuvwxyz123456") {
		t.Fatal("redacted payload fixture invalid")
	}
	var deps summaryMemoryDependencies
	_ = json.Unmarshal([]byte(hints[summaryMemoryHint]), &deps)
	if deps.Unverifiable || len(deps.References) != 1 || deps.References[0].ID != "numbered" {
		t.Fatalf("numbered source misparsed: %+v", deps)
	}
}

func TestSummaryMultiRefsOneFenceActualPayload(t *testing.T) {
	passages := []memory.Passage{
		{ID: "head", Content: "HEAD_FRAGMENT " + strings.Repeat("alpha evidence ", 300)},
		{ID: "middle", Content: "MIDDLE_WHOLE_BODY_MUST_BE_ABSENT"},
		{ID: "tail", Content: strings.Repeat("omega evidence ", 300) + " TAIL_FRAGMENT"},
	}
	refs := []recallReference{}
	for _, p := range passages {
		refs = append(refs, recallReference{p.ID, recallPassageHash(p)})
	}
	raw, _ := json.Marshal(refs)
	recall := strings.Replace(memory.FormatRetrieveSection(passages), "<auto-retrieve>", `<auto-retrieve refs="`+base64.RawURLEncoding.EncodeToString(raw)+`">`, 1)
	for _, nested := range []bool{false, true} {
		t.Run(istr(boolInt(nested)), func(t *testing.T) {
			block := llm.ContentBlock{Type: "text", Text: recall, Synthetic: true}
			messages := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{block}}}
			if nested {
				messages = []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "read", ToolName: "read"}}}, {Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "read", ToolResultBlocks: []llm.ContentBlock{{Type: "text", Text: "public head"}, block, {Type: "text", Text: "public tail"}}}}}}
			}
			p := &reviewRecallSummaryProvider{fact: "public summary"}
			cfg := DefaultCompactionConfig()
			cfg.MaxSummarizeInputTokens = 1400
			c := NewCompactor(cfg, "review", 100000, p)
			var hints map[string]string
			if _, err := c.summarizeWithSources(context.Background(), messages, "", "", nil, &hints); err != nil {
				t.Fatal(err)
			}
			payload := p.requests[0].Messages[0].Content[0].Text
			if strings.Contains(payload, passages[1].Content) {
				t.Fatal("middle body unexpectedly retained")
			}
			var deps summaryMemoryDependencies
			_ = json.Unmarshal([]byte(hints[summaryMemoryHint]), &deps)
			ids := map[string]bool{}
			for _, ref := range deps.References {
				ids[ref.ID] = true
			}
			if ids["middle"] {
				t.Fatalf("wholly removed middle dependency: %+v", deps)
			}
			if !nested && (!strings.Contains(payload, "HEAD_FRAGMENT alpha evidence") || !strings.Contains(payload, "omega evidence  TAIL_FRAGMENT") || !ids["head"] || !ids["tail"]) {
				t.Fatalf("partial payload lost dependencies: %+v payload=%s", deps, payload)
			}
			if nested && (!strings.Contains(payload, "HEAD_FRAGMENT alpha evidence") || !strings.Contains(payload, "omega evidence  TAIL_FRAGMENT") || !ids["head"] || !ids["tail"]) {
				t.Fatalf("nested transaction detail lost partially retained sources: %+v", deps)
			}
		})
	}
}

func TestSummaryPartiallyFittedSeedKeepsOpaqueDependencies(t *testing.T) {
	p := &reviewRecallSummaryProvider{fact: "new summary"}
	cfg := DefaultCompactionConfig()
	cfg.IterativeSummary = true
	cfg.MaxSummarizeInputTokens = 1400
	c := NewCompactor(cfg, "review", 100000, p)
	seed := "SEED_HEAD " + strings.Repeat("derived narrative ", 1000) + " SEED_TAIL"
	seedHints := summaryMemoryHints([]llm.Message{{Content: []llm.ContentBlock{{Type: "text", Synthetic: true, Text: formatRecall([]memory.Passage{{ID: "seed-source", Content: "source fact"}}, "")}}}})
	var hints map[string]string
	if _, err := c.summarizeWithSources(context.Background(), nil, seed, "", seedHints, &hints); err != nil {
		t.Fatal(err)
	}
	payload := p.requests[0].Messages[0].Content[0].Text
	if strings.Contains(payload, seed) || !strings.Contains(payload, "SEED_HEAD") || !strings.Contains(payload, "SEED_TAIL") {
		t.Fatal("seed not partially fitted in actual request")
	}
	if hints[summaryMemoryHint] != seedHints[summaryMemoryHint] {
		t.Fatal("partial opaque seed lost original dependency")
	}
}

func TestReviewMultiFenceOmittedSourceDoesNotInvalidateSummary(t *testing.T) {
	for _, cap := range []int{1200, 1400, 1600, 1800} {
		t.Run(istr(cap), func(t *testing.T) {
			mm, err := memory.NewMemoryManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			specs := []memory.Passage{
				{ID: "head", Content: "HEAD_SOURCE_START " + strings.Repeat("alpha evidence ", 80) + " HEAD_SOURCE_END", SourceSessionID: "head-origin"},
				{ID: "middle", Content: "MIDDLE_PRIVATE_FACT_WAS_COMPLETELY_EXCLUDED", SourceSessionID: "middle-origin"},
				{ID: "tail", Content: "TAIL_SOURCE_START " + strings.Repeat("omega evidence ", 80) + " TAIL_SOURCE_END", SourceSessionID: "tail-origin"},
			}
			for _, p := range specs {
				if err := mm.Archival().Insert(p); err != nil {
					t.Fatal(err)
				}
			}
			stored, err := mm.Search(memory.SearchOptions{SortBy: "recent"})
			if err != nil {
				t.Fatal(err)
			}
			byID := map[string]memory.Passage{}
			for _, p := range stored {
				byID[p.ID] = p
			}
			passages := []memory.Passage{byID["head"], byID["middle"], byID["tail"]}
			recall := formatRecall(passages, "")
			if memory.RecallTokens(recall) > 1500 {
				t.Fatalf("fixture recall above default cap: %d", memory.RecallTokens(recall))
			}
			history := unifiedPipelineHistory()
			history[0].Content = append(history[0].Content, llm.ContentBlock{Type: "text", Text: recall, Synthetic: true})
			provider := &reviewRecallSummaryProvider{fact: "Public task validation finished without private facts."}
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
			payload := provider.requests[0].Messages[0].Content[0].Text
			middleRetained := strings.Contains(payload, specs[1].Content)
			if middleRetained != (cap == 1800) {
				t.Fatal("fixture middle participation changed")
			}
			var deps summaryMemoryDependencies
			for _, m := range l.History() {
				for _, b := range m.Content {
					if strings.Contains(b.Text, provider.fact) {
						_ = json.Unmarshal([]byte(b.ProviderHint[summaryMemoryHint]), &deps)
					}
				}
			}
			hasMiddle := false
			for _, ref := range deps.References {
				hasMiddle = hasMiddle || ref.ID == "middle"
			}
			t.Logf("recall_estimate=%d summary_cap=%d head_body=%v middle_body=%v tail_body=%v middle_dependency=%v", memory.RecallTokens(recall), cap, strings.Contains(payload, "HEAD_SOURCE_START"), middleRetained, strings.Contains(payload, "TAIL_SOURCE_END"), hasMiddle)
			if cap == 1400 || cap == 1600 {
				if !strings.Contains(payload, "HEAD_SOURCE_START alpha evidence") || !strings.Contains(payload, "omega evidence  TAIL_SOURCE_END") {
					t.Fatal("fixture must retain actual head and tail body fragments")
				}
				ids := map[string]bool{}
				for _, ref := range deps.References {
					ids[ref.ID] = true
				}
				if !ids["head"] || !ids["tail"] || ids["middle"] {
					t.Fatalf("wrong retained sources: %+v", deps)
				}
			}
			if err := mm.DeleteSession("middle-origin"); err != nil {
				t.Fatal(err)
			}
			req := l.buildRequest(nil)
			safePresent := false
			for _, m := range req.Messages {
				for _, b := range m.Content {
					safePresent = safePresent || strings.Contains(b.Text, provider.fact)
				}
			}
			if hasMiddle != middleRetained {
				t.Fatalf("middle dependency=%v actual=%v", hasMiddle, middleRetained)
			}
			if safePresent == middleRetained {
				t.Fatal("summary withdrawal did not match actual middle participation")
			}
			if cap == 1400 || cap == 1600 {
				if err := mm.DeleteSession("head-origin"); err != nil {
					t.Fatal(err)
				}
				for _, m := range l.buildRequest(nil).Messages {
					for _, b := range m.Content {
						if strings.Contains(b.Text, provider.fact) {
							t.Fatal("summary survived withdrawal of partially retained source")
						}
					}
				}
			}
		})
	}
}
