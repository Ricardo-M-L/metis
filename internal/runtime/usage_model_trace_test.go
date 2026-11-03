package runtime

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestTracePreservesPerRequestUsageModel(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	adapter := NewTraceAdapter(store)
	adapter.SetSession("usage-models")
	adapter.OnEvent(agent.Event{Kind: agent.EventTokens, UsageModel: "first", InputTokens: 10})
	adapter.OnEvent(agent.Event{Kind: agent.EventTokens, UsageModel: "second", InputTokens: 20, OutputTokens: 3})
	adapter.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Agent", ToolUseID: "child"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTokens, UsageModel: "child-model", SubAgentParentID: "child", InputTokens: 5})
	got, err := store.ReadUsageEvents("usage-models")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].UsageModel != "first" || got[1].UsageModel != "second" || got[2].UsageModel != "child-model" || got[0].Turn != got[1].Turn || got[2].SubAgentOf != "child" {
		t.Fatalf("distinct model usage in one turn lost: %+v", got)
	}
	if got[1].Text != "input=20 output=3 cache_write=0 cache_read=0" {
		t.Fatalf("legacy text changed: %q", got[1].Text)
	}
}
