package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestMemoryRecallDisablesGenericIngestionSpill(t *testing.T) {
	passage := memory.Passage{
		ID: "large-recall", SourceSessionID: "large-recall-origin",
		Content: strings.Repeat("Cobalt archival detail. ", tools.DefaultMaxResultSizeChars/20+1),
	}
	tool := NewMemory(nil, memorySearchFixture{passages: []memory.Passage{passage}})
	result, err := tool.Execute(context.Background(), map[string]any{"action": "search", "query": "Cobalt"})
	if err != nil || result.IsError || len(result.Output) <= tools.DefaultMaxResultSizeChars {
		t.Fatalf("fixture did not exercise oversized recall: result=%+v, err=%v", result, err)
	}
	if tools.MaxResultSizeChars(tool) != tools.ResultSizeUnlimited {
		t.Fatal("Memory permits generic spill files that lose repository revocation provenance")
	}
	metadata := memorySearchMetadata(t, result)
	if len(metadata.References) != 1 || len(metadata.Passages) != 1 || metadata.Passages[0].Content != passage.Content {
		t.Fatal("oversized recall lost its authoritative source provenance")
	}
}
