package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

func summaryMemoryToolSearchFixture() (llm.ContentBlock, []memory.Passage) {
	passages := []memory.Passage{
		{ID: "first", Content: "FIRST_SOURCE " + strings.Repeat("first-public-detail ", 400)},
		{ID: "omitted", Content: "MIDDLE_SOURCE_NEVER_SENT_TO_SUMMARIZER"},
		{ID: "last", Content: strings.Repeat("last-public-detail ", 200) + " LAST_SOURCE"},
	}
	var body strings.Builder
	body.WriteString("Memory search for relevant history (3 hits):\n\n")
	var deps summaryMemoryDependencies
	for i, p := range passages {
		ref := recallReference{ID: p.ID, Hash: recallPassageHash(p)}
		deps.References = append(deps.References, ref)
		raw, _ := json.Marshal([]recallReference{ref})
		fmt.Fprintf(&body, "<auto-retrieve refs=\"%s\">\n[%d] [2026-11-13 · context] %s\n</auto-retrieve>\n\n", base64.RawURLEncoding.EncodeToString(raw), i+1, html.EscapeString(p.Content))
	}
	raw, _ := json.Marshal(deps)
	return llm.ContentBlock{Type: "tool_result", ToolUseID: "search-memory", ToolResult: body.String(), Synthetic: true,
		ProviderHint: map[string]string{summaryMemoryHint: string(raw), memoryToolObservationHint: "1"}}, passages
}

func memoryToolSummaryHints(t *testing.T, block llm.ContentBlock, maxInput int) (string, string) {
	t.Helper()
	provider := &rewindMemoryProvider{fact: "MIDDLE_SOURCE_NEVER_SENT_TO_SUMMARIZER"}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = maxInput
	c := NewCompactor(cfg, provider.ModelID(), 100000, provider)
	var hints map[string]string
	_, err := c.summarizeWithSources(context.Background(), []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolName: "Memory", ToolUseID: block.ToolUseID, ToolInput: map[string]any{"action": "search", "query": "relevant history"}}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{block}},
	}, "", "", nil, &hints)
	if err != nil || len(provider.requests) != 1 {
		t.Fatalf("summary requests=%d err=%v", len(provider.requests), err)
	}
	return provider.requests[0].Messages[0].Content[0].Text, hints[summaryMemoryHint]
}

func TestMemoryToolSummarySourcesFollowSerializerAndInputFitting(t *testing.T) {
	for _, maxInput := range []int{16000, 1400} {
		t.Run(fmt.Sprint(maxInput), func(t *testing.T) {
			block, passages := summaryMemoryToolSearchFixture()
			payload, hint := memoryToolSummaryHints(t, block, maxInput)
			if strings.Contains(payload, passages[1].Content) {
				t.Fatal("fixture did not completely clip the middle source")
			}
			var deps summaryMemoryDependencies
			if json.Unmarshal([]byte(hint), &deps) != nil {
				t.Fatal("missing summary source dependencies")
			}
			seen := map[string]bool{}
			for _, ref := range deps.References {
				seen[ref.ID] = true
			}
			if seen["omitted"] || !seen["first"] || !seen["last"] || deps.Unverifiable {
				t.Fatalf("dependencies include omitted source or lose partial sources: %+v", deps)
			}
			visible := map[string]string{"first": recallPassageHash(passages[0]), "last": recallPassageHash(passages[2])}
			if !validSummaryMemory(hint, "", visible, true) {
				t.Fatal("withdrawing an entirely omitted source invalidated retained summary")
			}
			delete(visible, "first")
			if validSummaryMemory(hint, "", visible, true) {
				t.Fatal("withdrawing a partially retained source did not invalidate summary")
			}
		})
	}
}

func TestMemoryToolCoreSummarySourcesFollowClippedOriginalLines(t *testing.T) {
	first := "FIRST_CORE_SOURCE " + strings.Repeat("first core detail ", 420)
	middle := "MIDDLE_SOURCE_NEVER_SENT_TO_SUMMARIZER"
	last := strings.Repeat("last core detail ", 180) + " LAST_CORE_SOURCE"
	body := strings.Join([]string{first, middle, last}, "\n")
	deps := summaryMemoryDependencies{ContextLines: []string{memoryLineHash(first), memoryLineHash(middle), memoryLineHash(last)}}
	raw, _ := json.Marshal(deps)
	block := llm.ContentBlock{Type: "tool_result", ToolUseID: "read-core", ToolResult: body + "\n\n[block user: 10000 chars]", Synthetic: true,
		ProviderHint: map[string]string{summaryMemoryHint: string(raw), memoryToolObservationHint: "1"}}
	payload, hint := memoryToolSummaryHints(t, block, 16000)
	if strings.Contains(payload, middle) {
		t.Fatal("fixture did not entirely omit middle Core line")
	}
	var got summaryMemoryDependencies
	if json.Unmarshal([]byte(hint), &got) != nil {
		t.Fatal("missing Core dependencies")
	}
	seen := map[string]bool{}
	for _, line := range got.ContextLines {
		seen[line] = true
	}
	if seen[memoryLineHash(middle)] || !seen[memoryLineHash(first)] || !seen[memoryLineHash(last)] {
		t.Fatalf("incorrect clipped Core dependencies: %+v", got)
	}
	retained := first + "\n" + last
	if !validSummaryMemory(hint, retained, nil, true) || validSummaryMemory(hint, last, nil, true) {
		t.Fatal("Core withdrawal does not distinguish omitted and partially retained lines")
	}
}

func TestMemoryToolSummaryEmptyProjectionAndUntrustedFencesHaveNoDependencies(t *testing.T) {
	block, _ := summaryMemoryToolSearchFixture()
	block.ToolResult = "[Memory result reduced; search again with a smaller limit.]"
	_, hint := memoryToolSummaryHints(t, block, 16000)
	if hint != "" {
		t.Fatal("zero-fact Memory recovery label inherited hidden source dependencies")
	}
	block, _ = summaryMemoryToolSearchFixture()
	block.Synthetic = false
	block.ProviderHint = nil
	_, hint = memoryToolSummaryHints(t, block, 16000)
	if hint != "" {
		t.Fatal("an ordinary tool's printed recall fence acquired host provenance")
	}
}
