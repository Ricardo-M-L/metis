package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// This provider only repeats a fact when the actual summary request included
// it; it never invents a withdrawn fact merely to make a test fail.
type rewindMemoryProvider struct {
	fact     string
	requests []llm.Request
}

func (p *rewindMemoryProvider) Name() string          { return "rewind-memory-test" }
func (p *rewindMemoryProvider) ModelID() string       { return "rewind-memory-test" }
func (p *rewindMemoryProvider) MaxContextTokens() int { return 100000 }
func (p *rewindMemoryProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("unexpected complete")
}
func (p *rewindMemoryProvider) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	p.requests = append(p.requests, req)
	answer := "Public summary without private source."
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if strings.Contains(block.Text, p.fact) {
				answer = "Derived: " + p.fact
			}
		}
	}
	return &fakeStream{events: []llm.StreamEvent{{Type: "text_delta", TextDelta: answer}, {Type: "message_stop"}}}, nil
}

func rewindMemoryFixture(t *testing.T) (*Loop, *memory.MemoryManager, *rewindMemoryProvider, memory.Passage) {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	passage := memory.Passage{ID: "rewind-source", Content: "Private Amber revision 736 belongs to this source.", SourceSessionID: "rewind-origin"}
	if err := mm.Archival().Insert(passage); err != nil {
		t.Fatal(err)
	}
	stored, err := mm.Search(memory.SearchOptions{SortBy: "recent"})
	if err != nil || len(stored) != 1 {
		t.Fatalf("source %v: %v", stored, err)
	}
	passage = stored[0]
	provider := &rewindMemoryProvider{fact: passage.Content}
	history := []llm.Message{msg(llm.RoleUser, "summarize this authorized task"), {Role: llm.RoleSystem, Content: []llm.ContentBlock{{Type: "text", Text: formatRecall([]memory.Passage{passage}, ""), Synthetic: true}}}, msg(llm.RoleAssistant, "public work result")}
	loop := &Loop{Memory: mm, Provider: provider, Model: provider.ModelID(), ContextWindow: 100000, Messages: history, Compactor: NewCompactor(DefaultCompactionConfig(), provider.ModelID(), 100000, provider)}
	return loop, mm, provider, passage
}

func TestRewindSummaryRejectsWithdrawnMemoryAtEveryBoundary(t *testing.T) {
	for _, timing := range []string{"before-prepare", "before-generate", "before-commit", "after-commit"} {
		t.Run(timing, func(t *testing.T) {
			loop, mm, provider, source := rewindMemoryFixture(t)
			withdraw := func() {
				t.Helper()
				if err := mm.DeleteSession("rewind-origin"); err != nil {
					t.Fatal(err)
				}
			}
			if timing == "before-prepare" {
				withdraw()
			}
			plan, err := loop.PrepareSummarizeFromTurn(1)
			if err != nil {
				t.Fatal(err)
			}
			if timing == "before-generate" {
				withdraw()
			}
			summary, err := loop.GenerateRewindSummary(context.Background(), plan)
			if timing == "before-generate" {
				if !errors.Is(err, ErrConversationChanged) || len(provider.requests) != 0 {
					t.Fatalf("withdrawn prepared source reached provider: requests=%d err=%v", len(provider.requests), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if timing == "before-prepare" && strings.Contains(provider.requests[0].Messages[0].Content[0].Text, source.Content) {
				t.Fatal("source withdrawn before prepare reached summary provider")
			}
			if timing == "before-commit" {
				withdraw()
			}
			persisted := false
			_, err = loop.CommitRewindSummaryWithPersist(plan, summary, func(history []llm.Message) error { persisted = true; return nil })
			if timing == "before-commit" {
				if !errors.Is(err, ErrConversationChanged) || persisted {
					t.Fatalf("withdrawn candidate was persisted: persisted=%v err=%v", persisted, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if timing == "after-commit" {
				boundary := loop.History()[len(loop.History())-1].Content[0]
				if boundary.ProviderHint[summaryMemoryHint] == "" {
					t.Error("rewind summary lost source provenance")
				}
				withdraw()
			}
			for _, m := range loop.buildRequest(nil).Messages {
				for _, b := range m.Content {
					if strings.Contains(b.Text, source.Content) {
						t.Errorf("withdrawn source survives rewind summary: %q", b.Text)
					}
				}
			}
		})
	}
}

func TestRewindSummaryPersistedSourceDependencySurvivesResume(t *testing.T) {
	loop, mm, provider, source := rewindMemoryFixture(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sid = "rewind-memory-resume"
	if err := store.WriteHeader(sid, provider.ModelID(), "public test system"); err != nil {
		t.Fatal(err)
	}
	plan, err := loop.PrepareSummarizeFromTurn(1)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := loop.GenerateRewindSummary(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	var cursor session.HistoryCursor
	if _, err := loop.CommitRewindSummaryWithPersist(plan, summary, func(history []llm.Message) error {
		return store.ReplaceHistoryAndMark(sid, history, &cursor)
	}); err != nil {
		t.Fatal(err)
	}
	// Reload from the production JSONL store into a fresh Loop, rather than
	// carrying any in-memory plan or summary maps into the resumed session.
	_, history, err := store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	boundary := history[len(history)-1].Content[0]
	if boundary.ProviderHint[summaryMemoryHint] == "" || !strings.Contains(boundary.Text, source.Content) {
		t.Fatal("persisted summary lost fact or provenance")
	}
	resumed := &Loop{Memory: mm, Provider: provider, Messages: history}
	if err := mm.DeleteSession("rewind-origin"); err != nil {
		t.Fatal(err)
	}
	foundRecoveryNotice := false
	for _, message := range resumed.buildRequest(nil).Messages {
		for _, block := range message.Content {
			if strings.Contains(block.Text, source.Content) {
				t.Fatal("resumed rewind summary retained withdrawn source")
			}
			foundRecoveryNotice = foundRecoveryNotice || block.Text == summaryMemoryInvalidated
		}
	}
	if !foundRecoveryNotice {
		t.Fatal("resumed revocation did not preserve explicit recovery notice")
	}
}

func TestSummaryCoreLineTracksRetainedDuplicateAfterClipping(t *testing.T) {
	fact := "Core policy says Amber revision 736 must stay private."
	// Fitting discards the first copy but keeps the last copy of the source.
	body := strings.Repeat("head public filler ", 100) + "\n" + fact + "\n" + strings.Repeat("middle filler ", 350) + "\n" + fact + "\nTAIL"
	sections, _ := json.Marshal([]llm.SystemSection{{Name: "memory_index", Body: body}})
	block := llm.ContentBlock{Type: "text", Text: "<dynamic_context_snapshot>\n\n[memory_index]\n" + body + "\n</dynamic_context_snapshot>", ProviderHint: map[string]string{dynamicCheckpointHint: "checkpoint", dynamicCheckpointHint + ".sections": string(sections)}}
	provider := &rewindMemoryProvider{fact: fact}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 1400
	compactor := NewCompactor(cfg, provider.ModelID(), 100000, provider)
	var hints map[string]string
	if _, err := compactor.summarizeWithSources(context.Background(), []llm.Message{{Role: llm.RoleSystem, Content: []llm.ContentBlock{block}}}, "", "", nil, &hints); err != nil {
		t.Fatal(err)
	}
	payload := provider.requests[0].Messages[0].Content[0].Text
	if strings.Count(payload, fact) != 1 {
		t.Fatalf("expected exactly one retained duplicate, got %d", strings.Count(payload, fact))
	}
	var deps summaryMemoryDependencies
	_ = json.Unmarshal([]byte(hints[summaryMemoryHint]), &deps)
	found := false
	for _, hash := range deps.ContextLines {
		found = found || hash == memoryLineHash(fact)
	}
	if !found {
		t.Fatal("retained duplicate lost memory dependency after first occurrence was clipped")
	}
	if validSummaryMemory(hints[summaryMemoryHint], strings.ReplaceAll(body, fact, ""), nil, true) {
		t.Fatal("summary survived withdrawal of the actual retained source")
	}
}

func TestSummaryCoreLineWhollyClippedDoesNotDonateDependency(t *testing.T) {
	fact := "Core policy says Amber revision 736 must stay private."
	body := strings.Repeat("head public filler ", 200) + "\n" + fact + "\n" + strings.Repeat("tail public filler ", 200)
	sections, _ := json.Marshal([]llm.SystemSection{{Name: "memory_index", Body: body}})
	block := llm.ContentBlock{Type: "text", Text: "<dynamic_context_snapshot>\n\n[memory_index]\n" + body + "\n</dynamic_context_snapshot>", ProviderHint: map[string]string{dynamicCheckpointHint: "checkpoint", dynamicCheckpointHint + ".sections": string(sections)}}
	provider := &rewindMemoryProvider{fact: fact}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 1400
	compactor := NewCompactor(cfg, provider.ModelID(), 100000, provider)
	var hints map[string]string
	if _, err := compactor.summarizeWithSources(context.Background(), []llm.Message{{Role: llm.RoleSystem, Content: []llm.ContentBlock{block}}}, "", "", nil, &hints); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(provider.requests[0].Messages[0].Content[0].Text, fact) {
		t.Fatal("fixture did not wholly exclude the source line")
	}
	var deps summaryMemoryDependencies
	_ = json.Unmarshal([]byte(hints[summaryMemoryHint]), &deps)
	for _, hash := range deps.ContextLines {
		if hash == memoryLineHash(fact) {
			t.Fatal("wholly omitted Core source became a summary dependency")
		}
	}
	if !validSummaryMemory(hints[summaryMemoryHint], strings.ReplaceAll(body, fact, ""), nil, true) {
		t.Fatal("withdrawing an omitted Core source invalidated unrelated retained sources")
	}
}

type rewindLocalMemoryProvider struct{ rewindMemoryProvider }

func (*rewindLocalMemoryProvider) DynamicContextCheckpoints() bool { return true }

func TestCoreDuplicateSummaryWithdrawalThroughCompaction(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fact := "Core policy says Amber revision 736 must stay private."
	core := map[string]string{
		"user":    strings.Repeat("public user notes ", 100),
		"system":  strings.Repeat("public system notes ", 110) + "\n" + fact,
		"working": strings.Repeat("public work notes ", 200),
		"summary": strings.Repeat("public summary notes ", 45) + "\n" + fact,
	}
	for label, body := range core {
		if err := mm.Core().UpdateBlock(label, body); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	body := mm.BuildContext()
	if strings.Count(body, fact) != 2 {
		t.Fatalf("core duplicate count %d", strings.Count(body, fact))
	}
	sections, _ := json.Marshal([]llm.SystemSection{{Name: "memory_index", Body: body}})
	checkpoint := llm.Message{Role: llm.RoleSystem, Content: []llm.ContentBlock{{Type: "text", Text: "<dynamic_context_snapshot>\n\n[memory_index]\n" + body + "\n</dynamic_context_snapshot>", ProviderHint: map[string]string{dynamicCheckpointHint: "checkpoint", dynamicCheckpointHint + ".sections": string(sections)}}}}
	provider := &rewindLocalMemoryProvider{rewindMemoryProvider{fact: fact}}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 1400
	cfg.RetainTokens = 200
	cfg.RetainMinMessages = 2
	cfg.RetainMinUserMessages = 1
	loop := &Loop{Memory: mm, Provider: provider, Model: provider.ModelID(), ContextWindow: 100000, Compactor: NewCompactor(cfg, provider.ModelID(), 100000, provider), Messages: append([]llm.Message{checkpoint}, unifiedPipelineHistory()...)}
	result, err := loop.CompactNow(context.Background(), CompactOptions{Force: true})
	if err != nil || !result.Applied {
		t.Fatalf("applied=%v err=%v", result.Applied, err)
	}
	payload := provider.requests[0].Messages[0].Content[0].Text
	if strings.Count(payload, fact) != 1 {
		t.Fatalf("fitting must retain exactly one Core copy; count=%d", strings.Count(payload, fact))
	}
	for _, label := range []string{"system", "summary"} {
		if err := mm.Core().UpdateBlock(label, strings.ReplaceAll(core[label], fact, "")); err != nil {
			t.Fatal(err)
		}
	}
	for _, message := range loop.buildRequest(nil).Messages {
		for _, block := range message.Content {
			if strings.Contains(block.Text, fact) {
				t.Fatal("derived summary retained withdrawn duplicated Core line")
			}
		}
	}
}
