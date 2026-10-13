package agent

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestInputContextBudgetDoesNotReserveIndependentOutputTwice(t *testing.T) {
	input := &Compactor{MaxContextTokens: 64000, MaxInputTokens: 64000, MaxOutputTokens: 128000, WindowKind: llm.ContextWindowInput}
	if got := input.effectiveInputCap(); got != 64000 {
		t.Fatalf("input-only budget = %d", got)
	}
	if err := validateContextCapabilitiesBudget(llm.ContextCapabilities{Window: 64000, WindowKind: llm.ContextWindowInput, MaxOutputTokens: 128000}, 128000); err != nil {
		t.Fatal(err)
	}
	if err := validateContextCapabilitiesBudget(llm.ContextCapabilities{Window: 64000, WindowKind: llm.ContextWindowInput, MaxOutputTokens: 32000}, 64000); !errors.Is(err, ErrContextCapabilitiesBudget) {
		t.Fatal("independent output ceiling was ignored")
	}
	total := *input
	total.WindowKind = llm.ContextWindowTotal
	total.MaxOutputTokens = 16000
	if total.effectiveInputCap() != 48000 {
		t.Fatal("total window lost output reservation")
	}
	input.MaxSummarizeInputTokens = 100000
	input.MaxSummaryTokens = 16000
	i, _ := input.summaryWireLimits()
	total.MaxSummarizeInputTokens = 100000
	total.MaxSummaryTokens = 16000
	j, _ := total.summaryWireLimits()
	if i-j != 16000 {
		t.Fatalf("summary double reservation: input=%d total=%d", i, j)
	}
}

func TestRebindInstallsInputOnlyContextSemanticsBeforeRefresh(t *testing.T) {
	for _, runtime := range []bool{false, true} {
		name := "provider model"
		if runtime {
			name = "complete runtime"
		}
		t.Run(name, func(t *testing.T) {
			old := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{
				Window: 200_000, WindowKind: llm.ContextWindowTotal, Source: "endpoint",
			}}
			loop := capabilitiesLoop(old)
			next := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{
				Window: 64_000, WindowKind: llm.ContextWindowInput,
				MaxInputTokens: 64_000, MaxOutputTokens: 128_000, Source: "endpoint",
			}}
			if runtime {
				loop.RebindProviderRuntime(next, "input-only-model", 4_000, "new system", nil)
			} else {
				loop.RebindProviderModel(next, "input-only-model")
			}
			// Inspect the binding immediately. A later metadata refresh must not
			// be required to repair the denominator after a model switch.
			caps, threshold, trigger := loop.ContextDetailsSnapshot()
			if caps.Window != 64_000 || caps.WindowKind != llm.ContextWindowInput ||
				caps.MaxInputTokens != 64_000 || caps.MaxOutputTokens != 128_000 {
				t.Fatalf("rebound context metadata = %+v", caps)
			}
			if loop.Compactor.WindowKind != caps.WindowKind || loop.Compactor.MaxContextTokens != caps.Window ||
				loop.Compactor.MaxInputTokens != caps.MaxInputTokens || loop.Compactor.MaxOutputTokens != 4_000 {
				t.Fatalf("rebound compactor disagrees with metadata: kind=%q window=%d input=%d output=%d", loop.Compactor.WindowKind, loop.Compactor.MaxContextTokens, loop.Compactor.MaxInputTokens, loop.Compactor.MaxOutputTokens)
			}
			if got := loop.Compactor.effectiveInputCap(); got != 64_000 {
				t.Fatalf("rebound input-only cap = %d, want 64000 without output reservation", got)
			}
			if want := int(float64(64_000) * threshold); trigger != want {
				t.Fatalf("rebound trigger = %d, want %d", trigger, want)
			}
			if next.refreshCalls != 0 {
				t.Fatal("model rebinding unexpectedly refreshed remote metadata")
			}
		})
	}
}

func TestContextBudgetErrorsFailWithoutOverflowRecovery(t *testing.T) {
	for _, err := range []error{ErrContextCapabilitiesBudget, llm.ErrContextBudgetPolicy, llm.ErrContextOutputBudget} {
		wrapped := fmt.Errorf("context window exceeds requested allowance: %w", err)
		if got := ClassifyError(wrapped); got != ErrInvalidRequest || got.Recovery() != RecoveryFailUser {
			t.Fatalf("budget error recovery = %v", got)
		}
	}
}

func TestInputOnlyContextUsageRetainsLegalReplyForNextCompaction(t *testing.T) {
	cases := []struct {
		name                     string
		input, cacheRead, output int
		wantValid                bool
		wantTokens               int
	}{
		{"separate input and output are legal", 50000, 0, 100000, true, 150000},
		{"cache prompt crossing input limit is invalid", 50000, 15000, 1000, false, 0},
		{"input itself crossing input limit is invalid", 65000, 0, 1000, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 64000, WindowKind: llm.ContextWindowInput, MaxInputTokens: 64000, MaxOutputTokens: 128000, Source: "endpoint"}}
			l := capabilitiesLoop(p)
			l.SyncContextCapabilities()
			primeActiveContextSnapshot(t, l, &usageTotals{in: c.input, cacheRead: c.cacheRead, out: c.output}, 0)
			if l.activeContext.valid != c.wantValid {
				t.Fatalf("valid=%t want=%t active=%+v estimate=%d window=%+v", l.activeContext.valid, c.wantValid, l.activeContext, l.EstimateContextTokens(), l.ContextMetadataSnapshot())
			}
			if c.wantValid && l.EstimateContextTokens() != c.wantTokens {
				t.Fatalf("active history=%d want=%d", l.EstimateContextTokens(), c.wantTokens)
			}
		})
	}
}

func TestWorkingBudgetDoesNotRejectReplyWithinPhysicalCapacity(t *testing.T) {
	p := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 64000, MaxWindow: 200000, MaxInputTokens: 180000, MaxOutputTokens: 128000, Source: "endpoint", RequestedWindow: 64000}}
	l := capabilitiesLoop(p)
	l.SyncContextCapabilities()
	primeActiveContextSnapshot(t, l, &usageTotals{in: 50000, out: 100000}, 0)
	if !l.activeContext.valid || l.EstimateContextTokens() != 150000 {
		t.Fatalf("valid reply rejected by software budget: %+v", l.activeContext)
	}
	if !l.Compactor.ShouldCompactTokens(150000) {
		t.Fatal("replay above working budget must trigger compaction")
	}
}

func TestContextValidationChangesFenceUsageWithoutCompactor(t *testing.T) {
	for _, field := range []string{"total limit", "output limit", "input semantics", "route"} {
		t.Run(field, func(t *testing.T) {
			p := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 64000, MaxWindow: 200000, MaxInputTokens: 180000, MaxOutputTokens: 128000, WindowKind: llm.ContextWindowTotal, RouteKey: "route-a"}}
			l := capabilitiesLoop(p)
			l.Compactor = nil
			l.SyncContextCapabilities()
			l.AppendUser("question")
			_, anchor := l.buildRequestWithContext(nil)
			p.mu.Lock()
			switch field {
			case "total limit":
				p.capabilities.MaxWindow = 250000
			case "output limit":
				p.capabilities.MaxOutputTokens = 32000
			case "input semantics":
				p.capabilities.WindowKind = llm.ContextWindowInput
			case "route":
				p.capabilities.RouteKey = "route-b"
			}
			p.mu.Unlock()
			l.SyncContextCapabilities()
			l.mu.Lock()
			l.Messages = append(l.Messages, msg(llm.RoleAssistant, "old response"))
			l.storeActiveContextSnapshotLocked(&usageTotals{in: 100, out: 10}, anchor)
			valid := l.activeContext.valid
			l.mu.Unlock()
			if valid {
				t.Fatal("usage captured before a validation change was installed")
			}
		})
	}
}
