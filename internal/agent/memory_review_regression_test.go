package agent

import (
	"context"
	"errors"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/session"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type reviewRecallSummaryProvider struct {
	fact     string
	sawFact  bool
	requests []llm.Request
}

func (p *reviewRecallSummaryProvider) Name() string          { return "review" }
func (p *reviewRecallSummaryProvider) ModelID() string       { return "review" }
func (p *reviewRecallSummaryProvider) MaxContextTokens() int { return 100000 }
func (p *reviewRecallSummaryProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("unexpected Complete")
}
func (p *reviewRecallSummaryProvider) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	p.requests = append(p.requests, req)
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.Text, p.fact) {
				p.sawFact = true
			}
		}
	}
	return &fakeStream{events: []llm.StreamEvent{{Type: "text_delta", TextDelta: "Important retained memory: " + p.fact}, {Type: "message_stop"}}}, nil
}
func TestCompactionSummaryRevocationRemovesDerivedFact(t *testing.T) {
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
	history := unifiedPipelineHistory()
	history[0].Content = append(history[0].Content, llm.ContentBlock{Type: "text", Text: formatRecall([]memory.Passage{p}, ""), Synthetic: true})
	provider := &reviewRecallSummaryProvider{fact: p.Content}
	cfg := DefaultCompactionConfig()
	cfg.RetainTokens = 200
	cfg.RetainMinMessages = 2
	cfg.RetainMinUserMessages = 1
	l := &Loop{Memory: mm, Provider: provider, Model: "review", ContextWindow: 100000, Messages: history, Compactor: NewCompactor(cfg, "review", 100000, provider)}
	compacted, err := l.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !compacted.Applied || !provider.sawFact {
		t.Fatalf("did not actually compact recall through summarizer: applied=%v sawFact=%v", compacted.Applied, provider.sawFact)
	}
	retained := false
	for _, m := range l.History() {
		for _, b := range m.Content {
			if strings.Contains(b.Text, "<auto-retrieve") {
				t.Fatal("original recall unexpectedly retained")
			}
			if strings.Contains(b.Text, p.Content) {
				retained = true
			}
		}
	}
	if !retained {
		t.Fatal("fixture summary did not retain source fact")
	}
	if err := mm.DeleteSession("source"); err != nil {
		t.Fatal(err)
	}
	req := l.buildRequest(nil)
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.Text, p.Content) {
				t.Errorf("revoked fact survives real compaction in model input: %q", b.Text)
			}
		}
	}
}
func TestReviewResetSessionDoesNotCarryPendingRevocation(t *testing.T) {
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fact := memory.Passage{ID: "old", Content: "Nebula old private session material.", SourceSessionID: "source"}
	if err := mm.Archival().Insert(fact); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Memory: mm, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Nebula"}, {Type: "text", Text: formatRecall([]memory.Passage{fact}, ""), Synthetic: true}}}}, HistoryCheckpoint: func([]llm.Message) error { return nil }}
	if err := mm.DeleteSession("source"); err != nil {
		t.Fatal(err)
	}
	l.buildRequest(nil)
	if l.memoryRevocationBefore == nil {
		t.Fatal("fixture did not produce pending revocation")
	}
	l.CompactionCheckpoint = func([]llm.Message, []llm.Message) error { return errors.New("disk full") }
	if err := l.checkpointRequestHistory(); err == nil {
		t.Fatal("expected failure")
	}
	l.ResetSession([]llm.Message{msg(llm.RoleUser, "new independent session")})
	storeDir := t.TempDir()
	store, err := session.NewStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeader("new-session", "review", "system"); err != nil {
		t.Fatal(err)
	}
	var cursor session.HistoryCursor
	l.CompactionCheckpoint = func(before, after []llm.Message) error {
		return store.CheckpointCompaction("new-session", before, after, &cursor)
	}
	l.HistoryCheckpoint = func(history []llm.Message) error { return store.CheckpointHistory("new-session", history, &cursor) }
	if err := l.checkpointRequestHistory(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(storeDir, "new-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), fact.Content) {
		t.Errorf("new session physical ledger includes revoked old-session fact: %q", fact.Content)
	}
}
func TestReviewRecallDoesNotDuplicateWithinOneRequestAfterRestore(t *testing.T) {
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := memory.Passage{ID: "a", Content: "Nebula uses Copper database."}
	if err := mm.Archival().Insert(p); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Memory: mm, AutoRetrieveK: 2, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Nebula"}}}}}
	l.prepareTurnMemory(context.Background())
	l.clearTurnMemoryLocked()
	l.prepareTurnMemory(context.Background())
	req := l.buildRequest(nil)
	total := strings.Count(req.System, p.Content)
	for _, s := range req.SystemSections {
		total += strings.Count(s.Body, p.Content)
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			total += strings.Count(b.Text, p.Content)
		}
	}
	if total != 1 {
		t.Errorf("same retained recall count=%d", total)
	}
}
