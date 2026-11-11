package agent

import (
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

type contextDisplayBudgetProvider struct {
	llm.Provider
	budget int
}

func (*contextDisplayBudgetProvider) Name() string          { return "display-budget" }
func (*contextDisplayBudgetProvider) ModelID() string       { return "budget-model" }
func (*contextDisplayBudgetProvider) MaxContextTokens() int { return 128000 }
func (p *contextDisplayBudgetProvider) DefaultOutputBudget() int {
	return p.budget
}

func TestContextDisplayKeepsDefaultOutputBudgetInNonblockingSnapshot(t *testing.T) {
	l := &Loop{Provider: &contextDisplayBudgetProvider{budget: 65536}, Model: "budget-model", ContextWindow: 128000}
	l.AppendUser("existing transcript")
	before := l.ContextDisplaySnapshot()
	if before.DefaultOutputBudgetTokens != 65536 || before.Used <= 0 {
		t.Fatalf("provider budget omitted from coherent context: %+v", before)
	}
	l.mu.Lock()
	read := make(chan ContextUsage, 1)
	go func() { read <- l.ContextDisplaySnapshot() }()
	select {
	case got := <-read:
		if got.DefaultOutputBudgetTokens != 65536 || got.Used != before.Used {
			t.Errorf("busy writer lost the cached provider budget: %+v", got)
		}
	case <-time.After(time.Second):
		t.Error("display budget blocked on loop writer")
	}
	l.mu.Unlock()
	l.RebindProviderModel(&contextDisplayBudgetProvider{budget: 2048}, "budget-two")
	after := l.ContextDisplaySnapshot()
	if after.DefaultOutputBudgetTokens != 2048 || after.Capabilities.Model != "budget-two" {
		t.Fatalf("provider switch mixed the previous budget with the new route: %+v", after)
	}
}
