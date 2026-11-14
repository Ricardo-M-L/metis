package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

func TestRecallBudgetDoesNotRefillOnRepeatedQuestion(t *testing.T) {
	passages := []memory.Passage{
		{ID: "a", Content: "Nebula " + strings.Repeat("数据库", 90)},
		{ID: "b", Content: "Nebula " + strings.Repeat("部署", 90)},
		{ID: "c", Content: "Nebula " + strings.Repeat("缓存", 90)},
	}
	first := novelRecall(passages, nil, "Nebula", 500, 600)
	if len(first) == 0 || len(first) >= len(passages) {
		t.Fatal("fixture must admit a proper nonempty budgeted subset")
	}
	history := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: formatRecall(first, ""), Synthetic: true}}}}
	if again := novelRecall(passages, history, "Nebula", 500, 600); len(again) != 0 {
		t.Fatal("repeat question admitted previously over-budget candidates")
	}
	if afterCompact := novelRecall(passages, nil, "Nebula", 500, 600); len(afterCompact) != len(first) {
		t.Fatal("compaction must make missing admitted bodies eligible again")
	}
}

func TestRecallCoverageRecognizesEscapedMemoryToolResult(t *testing.T) {
	p := memory.Passage{ID: "fact", Content: "Nebula stores <customer> records."}
	history := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolResult: formatRecall([]memory.Passage{p}, "")}}}}
	if got := novelRecall([]memory.Passage{p}, history, "Nebula", 800, 1500); len(got) != 0 {
		t.Fatal("tool-delivered memory was recalled again")
	}
}

type countedRecallSnapshot struct {
	*memory.MemoryManager
	snapshots, searches int
}

func (r *countedRecallSnapshot) RecallSnapshot() (string, string, error) {
	r.snapshots++
	return r.MemoryManager.RecallSnapshot()
}
func (r *countedRecallSnapshot) GuardRecallSnapshot(consume func(string, string, []memory.Passage) error) error {
	r.snapshots++
	return r.MemoryManager.GuardRecallSnapshot(consume)
}
func (r *countedRecallSnapshot) AutoRetrieveCandidates(q string, k int) []memory.Passage {
	r.searches++
	return r.MemoryManager.AutoRetrieveCandidates(q, k)
}

func TestDemandMemoryPrepareUsesOneAuthorityReadAndNoSearch(t *testing.T) {
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &countedRecallSnapshot{MemoryManager: mm}
	l := &Loop{Memory: r, AutoRetrieveK: 0, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "你好"}}}}}
	l.prepareTurnMemory(context.Background())
	if r.snapshots != 1 || r.searches != 0 {
		t.Fatalf("authority reads=%d searches=%d, want 1 and 0", r.snapshots, r.searches)
	}
}
