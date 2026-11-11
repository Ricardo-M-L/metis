package llm

import (
	"context"
	"testing"
)

type outputBudgetFixture struct {
	Provider
	budget int
}

func (p outputBudgetFixture) DefaultOutputBudget() int { return p.budget }

func TestProviderDefaultOutputBudgetStaysReadOnlyAndPreservesUnknown(t *testing.T) {
	if got := ProviderDefaultOutputBudget(nil); got != 0 {
		t.Fatalf("nil provider budget = %d", got)
	}
	unknown := struct{ Provider }{}
	if got := ProviderDefaultOutputBudget(unknown); got != 0 {
		t.Fatalf("unknown provider claimed output budget = %d", got)
	}
	for _, amount := range []int{32_000, 0, -1} {
		base := outputBudgetFixture{budget: amount}
		bound := BindExecutionProvider(base, context.Background())
		if got := ProviderDefaultOutputBudget(bound); got != max(amount, 0) {
			t.Fatalf("bound budget = %d, want %d", got, max(amount, 0))
		}
		if base.budget != amount {
			t.Fatal("reading a budget changed the adapter")
		}
	}
}
