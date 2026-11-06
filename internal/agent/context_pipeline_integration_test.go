package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// Exercise the real loop, tool execution, first-ingestion projection and durable
// session boundary together. A helper-only truncation test cannot establish
// that the provider actually receives the retained projection after fsync.
func TestContextPipelineBudgetCheckpointAndResume(t *testing.T) {
	l, p, step := newContinuationTestLoop(8, 1)
	l.SpillDir = t.TempDir()
	l.ToolResultTokenLimit = 900
	l.ToolBatchTokenLimit = 1400
	step.output = strings.Repeat("complete original tool evidence\n", 1200)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "context-pipeline"
	if err := store.WriteHeader(id, "test-model", "sys"); err != nil {
		t.Fatal(err)
	}
	var cursor session.HistoryCursor
	l.HistoryCheckpoint = func(history []llm.Message) error { return store.CheckpointHistory(id, history, &cursor) }
	checked := false
	p.before = func(n int, req llm.Request) {
		if n != 2 {
			return
		}
		_, saved, err := store.Load(id)
		if err != nil {
			t.Fatalf("load before provider continuation: %v", err)
		}
		if !reflect.DeepEqual(saved, req.Messages) {
			t.Fatal("provider continued before exact projected history became durable")
		}
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Type == "tool_result" {
					if b.ToolResult == step.output {
						t.Fatal("full oversized observation entered provider request")
					}
					if batchTokens([]llm.ContentBlock{b}) > l.ToolResultTokenLimit {
						t.Fatal("provider observation exceeds first-ingestion budget")
					}
					checked = true
				}
			}
		}
	}
	out := make(chan Event, 256)
	if err := l.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("did not observe projected provider continuation")
	}
	if err := l.HistoryCheckpoint(l.History()); err != nil {
		t.Fatal(err)
	}
	_, restored, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, l.History()) {
		t.Fatal("resume changed the exact retained projection")
	}
	entries, err := os.ReadDir(l.SpillDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(l.SpillDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) == step.output {
			found = true
		}
	}
	if !found {
		t.Fatal("original observation cannot be recovered from retained storage")
	}
}
