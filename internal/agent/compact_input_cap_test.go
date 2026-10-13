package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestCompactorEffectiveInputCapUsesAdvertisedInputCeiling(t *testing.T) {
	compactor := NewCompactor(DefaultCompactionConfig(), "model", 1_050_000, nil)
	compactor.MaxOutputTokens = 128_000
	compactor.MaxInputTokens = 922_000
	if got := compactor.effectiveInputCap(); got != 922_000 {
		t.Fatalf("effective input cap=%d, want advertised input ceiling 922000", got)
	}
	if got := compactor.TriggerTokens(); got != int(float64(922_000)*compactor.Threshold) {
		t.Fatalf("compression trigger=%d ignores input ceiling", got)
	}
}

func TestCompactorFullCatalogInputBudgetRetainsConfiguredThreshold(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.Threshold = .9
	compactor := NewCompactor(cfg, "full-profile-model", 1_050_000, nil)
	compactor.MaxInputTokens = 922_000
	compactor.MaxOutputTokens = 8_192
	if cap := compactor.effectiveInputCap(); cap != 922_000 {
		t.Fatalf("input cap=%d, want 922000", cap)
	}
	if trigger := compactor.TriggerTokens(); trigger != 829_800 {
		t.Fatalf("compression trigger=%d, want original .9 threshold applied to full input cap", trigger)
	}
	if compactor.ShouldCompactTokens(300_000) {
		t.Fatal("300k input was prematurely compacted under a resolved full model capacity")
	}
}

func TestContextCapabilitiesIndependentInputCeilingUpdatesInstalledBinding(t *testing.T) {
	provider := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 200_000, Source: "fallback"}}
	loop := capabilitiesLoop(provider)
	before, revision := loop.Compactor, loop.routingRevision
	provider.mu.Lock()
	provider.capabilities.MaxInputTokens = 100_000
	provider.mu.Unlock()
	loop.SyncContextCapabilities()
	capabilities, threshold, trigger := loop.ContextDetailsSnapshot()
	if capabilities.Window != 200_000 || capabilities.DefaultWindow != 0 || capabilities.Source != "fallback" || capabilities.MaxInputTokens != 100_000 {
		t.Fatalf("input limit was converted into an invented context window: %+v", capabilities)
	}
	if loop.Compactor == before || loop.Compactor.MaxInputTokens != 100_000 || loop.routingRevision == revision || loop.Compactor.MaxOutputTokens != 4_000 {
		t.Fatal("input-only metadata did not replace the coherent binding without changing output budget")
	}
	if trigger > 90_000 || trigger != int(float64(100_000)*threshold) || loop.Compactor.SnipMaxToolResultChars != 1_500 {
		t.Fatalf("compression ignored the independent input ceiling: trigger=%d threshold=%v chars=%d", trigger, threshold, loop.Compactor.SnipMaxToolResultChars)
	}
}

func TestContextCapabilitiesAdvertisedTotalInputOutputStaySeparate(t *testing.T) {
	provider := &liveContextCapabilitiesProvider{
		capabilities: llm.ContextCapabilities{Window: 1_050_000, Source: "fallback"},
		next:         llm.ContextCapabilities{Window: 1_050_000, DefaultWindow: 1_050_000, MaxInputTokens: 922_000, MaxOutputTokens: 128_000, Source: "endpoint"},
	}
	loop := capabilitiesLoop(provider)
	loop.Compactor.MaxOutputTokens = 128_000
	if err := loop.RefreshCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	capabilities, threshold, trigger := loop.ContextDetailsSnapshot()
	if capabilities != provider.next || loop.Compactor.MaxContextTokens != 1_050_000 || loop.Compactor.MaxInputTokens != 922_000 || loop.Compactor.MaxOutputTokens != 128_000 {
		t.Fatalf("total/input/output limits were conflated: metadata=%+v compactor=%+v", capabilities, loop.Compactor)
	}
	if trigger != int(float64(922_000)*threshold) {
		t.Fatalf("trigger=%d does not obey input limit", trigger)
	}
}

func TestCompactorSummaryRequestObeysIndependentInputCeiling(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.SummaryTimeoutSeconds = 0
	provider := &fakeSummarizer{}
	compactor := NewCompactor(cfg, "model", 200_000, provider)
	compactor.MaxInputTokens = 2_048
	inputCap, outputCap := compactor.summaryWireLimits()
	if inputCap > 2_048 || outputCap != cfg.MaxSummaryTokens {
		t.Fatalf("summary limits=input %d output %d", inputCap, outputCap)
	}
	if _, err := compactor.summarizeWithInstructions(context.Background(), []llm.Message{
		msg(llm.RoleUser, strings.Repeat("old input ", 10_000)),
		msg(llm.RoleAssistant, strings.Repeat("old result ", 10_000)),
	}, "", ""); err != nil {
		t.Fatal(err)
	}
	inputTokens := estimateStringTokens(provider.lastReq.System) + estimateTokens(provider.lastReq.Messages)
	if inputTokens > inputCap {
		t.Fatalf("summary request input=%d exceeds advertised-bound cap %d", inputTokens, inputCap)
	}
	if provider.lastReq.MaxTokens != outputCap {
		t.Fatal("input limit changed configured summary output")
	}
}
