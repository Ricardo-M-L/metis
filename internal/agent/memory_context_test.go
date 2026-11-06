package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

func TestRecallCoverageRebuildsFromActiveBody(t *testing.T) {
	p := memory.Passage{ID: "one", Content: "Nebula release uses Copper database and Quartz deployment."}
	query := "Nebula release"
	partial := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Nebula release uses Copper database"}}}}
	if got := novelRecall([]memory.Passage{p}, partial, query, 800, 1500); len(got) != 1 {
		t.Fatal("partial coverage suppressed whole body")
	}
	full := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: formatRecall([]memory.Passage{p}, "generation")}}}}
	if got := novelRecall([]memory.Passage{p}, full, query, 800, 1500); len(got) != 0 {
		t.Fatal("full body injected again")
	}
	p.UseCount = 999
	p.UpdatedAt = "changed metadata"
	if got := novelRecall([]memory.Passage{p}, full, query, 800, 1500); len(got) != 0 {
		t.Fatal("metadata defeated coverage")
	}
	if got := novelRecall([]memory.Passage{p}, nil, query, 800, 1500); len(got) != 1 {
		t.Fatal("compacted body should be recalled again")
	}
}

func TestRecallPreparePreservesPrefixAndRevokesState(t *testing.T) {
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := mm.Archival().Insert(memory.Passage{ID: "nebula", Content: "Nebula release uses Copper database.", SourceSessionID: "source-session"}); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Memory: mm, AutoRetrieveK: 3, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Nebula release"}}}}}
	l.prepareTurnMemory(context.Background())
	if !l.turnMemoryRecallAttached {
		t.Fatal("recall not attached")
	}
	prefix := l.Messages[0].Content[1].Text
	l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "provider_state"}}}, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Nebula release"}}})
	l.clearTurnMemoryLocked()
	l.prepareTurnMemory(context.Background())
	if l.Messages[0].Content[1].Text != prefix {
		t.Fatal("access metadata changed prefix")
	}
	if l.turnMemoryRecallAttached {
		t.Fatal("duplicate recall on next turn")
	}
	if err := mm.Archival().Insert(memory.Passage{ID: "unrelated", Content: "Unrelated project fact."}); err != nil {
		t.Fatal(err)
	}
	l.buildRequest(nil)
	if l.Messages[0].Content[1].Text != prefix {
		t.Fatal("unrelated memory insertion rewrote prefix")
	}
	if err := mm.DeleteSession("source-session"); err != nil {
		t.Fatal(err)
	}
	l.buildRequest(nil)
	for _, m := range l.Messages {
		for _, b := range m.Content {
			if b.Type == "provider_state" || strings.Contains(b.Text, "<auto-retrieve") {
				t.Fatal("stale recall/provider state survived generation change")
			}
		}
	}
}
