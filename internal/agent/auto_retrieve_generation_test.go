package agent

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

type recallMutationRepository struct {
	memory.Repository
	manager *memory.MemoryManager
}

func (r *recallMutationRepository) RecallSnapshot() (string, string, error) {
	return r.manager.RecallSnapshot()
}
func (r *recallMutationRepository) AutoRetrieveCandidates(query string, k int) []memory.Passage {
	hits := r.Repository.AutoRetrieveCandidates(query, k)
	_ = r.Repository.DeleteSession("recall-race")
	return hits
}

func TestRecallAttachmentRejectsChangedGeneration(t *testing.T) {
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := mm.Archival().Insert(memory.Passage{ID: "race", Content: "Nebula deployment", SourceSessionID: "recall-race"}); err != nil {
		t.Fatal(err)
	}
	repository := &recallMutationRepository{Repository: mm, manager: mm}
	l := &Loop{Memory: repository, AutoRetrieveK: 3, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Nebula deployment"}}}}}
	l.prepareTurnMemory(context.Background())
	if l.turnMemoryRecall != "" || l.turnMemoryRecallAttached {
		t.Fatal("revoked candidate attached after generation changed")
	}
}

func TestRecallLegacyRepositoryRetainsHistoricalRecall(t *testing.T) {
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Embedding only the public API intentionally hides the optional snapshot.
	repository := struct{ memory.Repository }{mm}
	text := "<auto-retrieve>legacy plugin recall</auto-retrieve>"
	l := &Loop{Memory: repository, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: text, Synthetic: true}}}}}
	l.validateMemorySnapshotLocked()
	if len(l.Messages[0].Content) != 1 || l.Messages[0].Content[0].Text != text {
		t.Fatal("plugin recall silently erased")
	}
}
