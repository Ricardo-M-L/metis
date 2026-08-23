package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestShouldCompactTokensUsesMessagePolicy(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.Threshold = 0.5
	cfg.MinimumTokens = 400
	c := NewCompactor(cfg, "test", 1_000, &fakeSummarizer{})

	if c.ShouldCompactTokens(399) {
		t.Fatal("numeric estimate below minimum crossed the policy")
	}
	if !c.ShouldCompactTokens(500) {
		t.Fatal("numeric estimate at trigger did not cross the policy")
	}
}

func TestEstimateRequestContextTokensIncludesStateAndTools(t *testing.T) {
	loop := &Loop{
		System: "this legacy string must be ignored when typed sections exist " + strings.Repeat("x", 4_000),
		SystemSections: []llm.SystemSection{
			{Name: "base", Body: strings.Repeat("base ", 300)},
		},
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}},
		CurrentStateSections: func() []llm.SystemSection {
			return []llm.SystemSection{{Name: "runtime", Body: strings.Repeat("state ", 120)}}
		},
		BypassNextCache: true,
	}
	specs := []llm.ToolSpec{{
		Name: "Read", Description: strings.Repeat("read files ", 100),
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
	}}
	historyOnly := estimateTokens(loop.Messages)
	withoutTools := loop.EstimateRequestContextTokens(nil)
	got := loop.EstimateRequestContextTokens(specs)
	if got <= historyOnly {
		t.Fatalf("request pressure = %d, history only = %d; system/state/tools were not counted", got, historyOnly)
	}
	if got <= withoutTools {
		t.Fatalf("normal request pressure omitted tools: with=%d without=%d", got, withoutTools)
	}
	if cached := loop.EstimateContextTokens(); cached != got {
		t.Fatalf("cached context estimate = %d, want preflight %d", cached, got)
	}
	if !loop.BypassNextCache {
		t.Fatal("pressure preflight consumed one-shot request flags")
	}

	// Typed sections are authoritative on providers that support them. A huge
	// legacy System mirror must not be double-counted.
	loop.System = ""
	withoutLegacyMirror := loop.EstimateRequestContextTokens(specs)
	if withoutLegacyMirror != got {
		t.Fatalf("typed sections did not take precedence: with mirror=%d without=%d", got, withoutLegacyMirror)
	}
}

func TestEstimateRequestContextTokensRescueOmitsToolsWithoutConsumingFlag(t *testing.T) {
	loop := &Loop{
		System:        strings.Repeat("system ", 80),
		Messages:      []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "finish the answer"}}}},
		rescueNoTools: true,
	}
	specs := []llm.ToolSpec{{
		Name:        "LargeTool",
		Description: strings.Repeat("large schema description ", 400),
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"payload": map[string]any{"type": "string", "description": strings.Repeat("payload ", 400)},
		}},
	}}

	want := loop.EstimateRequestContextTokens(nil)
	got := loop.EstimateRequestContextTokens(specs)
	if got != want {
		t.Fatalf("rescue pressure included tools: got=%d want(without tools)=%d", got, want)
	}
	if !loop.rescueNoTools {
		t.Fatal("pressure preflight consumed rescueNoTools")
	}
	requestWithoutTools := loop.rescueNoToolsSnapshot()
	req := loop.buildRequest(specs)
	if len(req.Tools) != 0 {
		t.Fatalf("rescue request retained %d tools", len(req.Tools))
	}
	retry := loop.buildRequestForRetry(specs, requestWithoutTools)
	if len(retry.Tools) != 0 {
		t.Fatalf("rescue overflow retry restored %d tools", len(retry.Tools))
	}
}

func TestCompactNowAutoUsesFullRequestPressure(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.Threshold = 0.5
	cfg.MinimumTokens = 0
	cfg.MaxSummarizeInputTokens = 0
	cfg.RetainTokens = 24
	cfg.RetainMinMessages = 1
	cfg.RetainMinUserMessages = 1
	c := NewCompactor(cfg, "test", 1_000, p)
	messages := make([]llm.Message, 0, 12)
	for i := 0; i < 6; i++ {
		messages = append(messages,
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "request " + strings.Repeat(string(rune('a'+i)), 100)}}},
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "answer " + strings.Repeat(string(rune('k'+i)), 100)}}},
		)
	}
	loop := &Loop{Compactor: c, Messages: messages}
	if c.ShouldCompact(loop.Messages) {
		t.Fatal("precondition: history alone already crosses trigger")
	}
	result, err := loop.CompactNow(context.Background(), CompactOptions{
		Trigger:                "auto",
		EstimatedContextTokens: 600,
	})
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if !result.Applied || p.calls != 1 {
		t.Fatalf("wire-pressure compaction skipped: applied=%v calls=%d", result.Applied, p.calls)
	}
}

func newIrreduciblePressureLoop() (*Loop, *fakeSummarizer, *Compactor) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.Threshold = 0.5
	cfg.MinimumTokens = 0
	cfg.MaxSummarizeInputTokens = 0
	cfg.RetainTokens = 24
	cfg.RetainMinMessages = 5
	cfg.RetainMinUserMessages = 1
	c := NewCompactor(cfg, "test", 1_000, p)

	messages := make([]llm.Message, 0, 16)
	for i := 0; i < 8; i++ {
		messages = append(messages,
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "request " + strings.Repeat(string(rune('a'+i)), 100)}}},
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "answer " + strings.Repeat(string(rune('k'+i)), 100)}}},
		)
	}
	loop := &Loop{
		Compactor: c,
		System:    strings.Repeat("fixed-overhead ", 220),
		Messages:  messages,
	}
	return loop, p, c
}

func TestMaybeCompactIrreducibleOverheadWaitsForMaterialHistoryGrowth(t *testing.T) {
	loop, p, c := newIrreduciblePressureLoop()
	loop.maybeCompact(context.Background(), nil)
	if p.calls != 1 {
		t.Fatalf("first pressure pass made %d summary calls, want 1", p.calls)
	}
	if got := estimateTokens(loop.History()) + int(loop.requestOverheadTokens.Load()); got < c.TriggerTokens() {
		t.Fatalf("test setup did not leave irreducible pressure: post=%d trigger=%d", got, c.TriggerTokens())
	}

	loop.maybeCompact(context.Background(), nil)
	loop.maybeCompact(context.Background(), nil)
	if p.calls != 1 {
		t.Fatalf("unchanged compacted history was summarized again: calls=%d", p.calls)
	}
	if c.CircuitTripped() || c.consecutiveFailures != 0 {
		t.Fatalf("suppressed repeats damaged circuit: failures=%d tripped=%v", c.consecutiveFailures, c.CircuitTripped())
	}
	loop.AppendUser("tiny status update")
	loop.maybeCompact(context.Background(), nil)
	if p.calls != 1 {
		t.Fatalf("insubstantial history growth re-armed compaction: calls=%d", p.calls)
	}

	// A genuinely new chunk of conversation re-arms automatic compaction.
	loop.AppendUser("new substantial request " + strings.Repeat("u", 12_000))
	loop.mu.Lock()
	loop.Messages = append(loop.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: strings.Repeat("a", 12_000)}}})
	loop.mu.Unlock()
	loop.maybeCompact(context.Background(), nil)
	if p.calls != 2 {
		t.Fatalf("material history growth did not re-arm compaction: calls=%d want=2", p.calls)
	}
}

func TestMaybeCompactMaterialOverheadGrowthRearmsPressureWatermark(t *testing.T) {
	loop, p, _ := newIrreduciblePressureLoop()
	loop.maybeCompact(context.Background(), nil)
	if p.calls != 1 {
		t.Fatalf("first pressure pass made %d summary calls, want 1", p.calls)
	}

	loop.System += strings.Repeat("new-runtime-overhead ", 2_000)
	loop.maybeCompact(context.Background(), nil)
	if p.calls != 2 {
		t.Fatalf("material overhead growth did not permit one retry: calls=%d want=2", p.calls)
	}
}

func TestAutoCompactWatermarkRebasesAfterManualReplacement(t *testing.T) {
	loop, p, _ := newIrreduciblePressureLoop()
	loop.maybeCompact(context.Background(), nil)
	if !loop.autoCompactPressurePinned {
		t.Fatal("precondition: irreducible pressure did not arm watermark")
	}

	loop.AppendUser("manual compact input " + strings.Repeat("m", 2_000))
	result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
	if err != nil || !result.Applied {
		t.Fatalf("manual CompactNow result=%+v err=%v", result, err)
	}
	manualBaseline := estimateTokens(loop.History())
	loop.maybeCompact(context.Background(), nil)
	if p.calls != 2 {
		t.Fatalf("manual replacement was immediately auto-summarized: calls=%d want=2", p.calls)
	}
	if got := loop.autoCompactHistoryTokens; got != manualBaseline {
		t.Fatalf("manual replacement left stale watermark: got=%d want=%d", got, manualBaseline)
	}

	loop.AppendUser("material work after manual compact " + strings.Repeat("n", 12_000))
	loop.maybeCompact(context.Background(), nil)
	if p.calls != 3 {
		t.Fatalf("material history after manual replacement did not re-arm: calls=%d want=3", p.calls)
	}
}

func TestStaleAutoCompactResultCannotRearmResetOrRestore(t *testing.T) {
	history := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "same logical history"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "same reply"}}},
	}
	tests := []struct {
		name  string
		reset func(*Loop)
	}{
		{name: "restore", reset: func(loop *Loop) { loop.Restore(history) }},
		{name: "reset-session", reset: func(loop *Loop) { loop.ResetSession(history) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeSummarizer{}
			cfg := DefaultCompactionConfig()
			cfg.Threshold = 0.5
			cfg.MinimumTokens = 0
			loop := &Loop{Compactor: NewCompactor(cfg, "test", 1_000, p)}
			loop.Restore(history)
			generation := loop.autoCompactGenerationSnapshot()
			result := CompactResult{
				Applied: true, BeforeTokens: 400, AfterTokens: estimateTokens(history), History: cloneMessages(history),
			}

			// Replace with identical content: history equality alone cannot identify
			// that a new session/history boundary won after CompactNow returned.
			tt.reset(loop)
			loop.noteAutoCompactPressure(result, 700, generation)
			if loop.autoCompactPressurePinned || loop.autoCompactHistoryTokens != 0 {
				t.Fatal("stale auto-compaction result re-armed state after reset/restore")
			}
		})
	}
}

func TestForcedCompactionPathsRebasePressureWatermark(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Loop) bool
	}{
		{
			name: "overflow",
			run: func(loop *Loop) bool {
				return loop.tryRecoverOverflow(context.Background(), errors.New("context_length_exceeded"), nil)
			},
		},
		{
			name: "second-wind",
			run: func(loop *Loop) bool {
				return loop.compactForSecondWind(context.Background(), nil)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loop, p, _ := newIrreduciblePressureLoop()
			loop.EstimateRequestContextTokens(nil)
			if !tt.run(loop) {
				t.Fatal("forced compaction did not apply")
			}
			if p.calls != 1 {
				t.Fatalf("summary calls=%d want=1", p.calls)
			}
			if !loop.autoCompactPressurePinned {
				t.Fatal("irreducible pressure did not arm replacement watermark")
			}
			if got, want := loop.autoCompactHistoryTokens, estimateTokens(loop.History()); got != want {
				t.Fatalf("replacement watermark=%d want current history=%d", got, want)
			}
			if loop.autoCompactHistoryRevision != loop.historyRevision {
				t.Fatalf("replacement revision=%d want current=%d", loop.autoCompactHistoryRevision, loop.historyRevision)
			}
		})
	}
}
