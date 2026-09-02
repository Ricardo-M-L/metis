package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type activeContextTestStream struct {
	events []llm.StreamEvent
	next   int
}

func (s *activeContextTestStream) Recv() (llm.StreamEvent, error) {
	if s.next >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := s.events[s.next]
	s.next++
	return event, nil
}

func (*activeContextTestStream) Close() error { return nil }

type activeContextTestProvider struct {
	name      string
	model     string
	responses [][]llm.StreamEvent
	next      int
}

func (p *activeContextTestProvider) Name() string        { return p.name }
func (p *activeContextTestProvider) ModelID() string     { return p.model }
func (*activeContextTestProvider) MaxContextTokens() int { return 128_000 }
func (*activeContextTestProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("active-context test expects streaming")
}
func (p *activeContextTestProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	if p.next >= len(p.responses) {
		return nil, errors.New("unexpected active-context provider call")
	}
	events := append([]llm.StreamEvent(nil), p.responses[p.next]...)
	p.next++
	return &activeContextTestStream{events: events}, nil
}

func activeContextResponse(text string, input, cacheCreate, cacheRead, output int) []llm.StreamEvent {
	return []llm.StreamEvent{
		{Type: "message_start", InputTokens: input, CacheCreationInputTokens: cacheCreate, CacheReadInputTokens: cacheRead},
		{Type: "text_delta", TextDelta: text},
		{Type: "message_delta", StopReason: "end_turn", OutputTokens: output},
		{Type: "message_stop"},
	}
}

func runActiveContextTurn(t *testing.T, loop *Loop, prompt string) {
	t.Helper()
	loop.AppendUser(prompt)
	out := make(chan Event, 32)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run(%q): %v", prompt, err)
	}
}

func TestActiveContextTwoProviderResponsesReplaceRatherThanAccumulate(t *testing.T) {
	provider := &activeContextTestProvider{
		name:  "context-test",
		model: "context-model",
		responses: [][]llm.StreamEvent{
			activeContextResponse("first answer", 600, 100, 200, 50),
			activeContextResponse("second answer", 900, 150, 300, 75),
		},
	}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "system", 4)
	loop.Model = provider.ModelID()

	runActiveContextTurn(t, loop, "first question")
	first := 600 + 100 + 200 + 50
	if got := loop.EstimateContextTokens(); got != first {
		t.Fatalf("first active context = %d, want %d", got, first)
	}

	runActiveContextTurn(t, loop, "second question")
	second := 900 + 150 + 300 + 75
	if got := loop.EstimateContextTokens(); got != second {
		t.Fatalf("second active context = %d, want replacement %d (not cumulative %d)", got, second, first+second)
	}
}

func primeActiveContextSnapshot(t *testing.T, loop *Loop, usage *usageTotals, overhead int) int {
	t.Helper()
	loop.mu.Lock()
	loop.Messages = []llm.Message{{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "run the tool"}},
	}}
	req := llm.Request{
		Model:    loop.Model,
		System:   strings.Repeat("x", overhead*4),
		Messages: append([]llm.Message(nil), loop.Messages...),
	}
	anchor := loop.contextRequestAnchorLocked(loop.Provider, req)
	loop.Messages = append(loop.Messages, llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "tool-1", ToolName: "Read"}},
	})
	loop.storeActiveContextSnapshotLocked(usage, anchor)
	loop.mu.Unlock()
	tokens, ok := disjointUsageTokens(usage)
	if !ok {
		t.Fatal("test usage did not produce a valid snapshot")
	}
	return tokens
}

func TestActiveContextCountsPostResponseToolResultExactlyOnce(t *testing.T) {
	provider := &activeContextTestProvider{name: "context-test", model: "context-model"}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "", 2)
	loop.Model = provider.ModelID()
	base := primeActiveContextSnapshot(t, loop, &usageTotals{in: 1_000, cacheRead: 250, out: 40}, 17)

	toolResult := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{
		Type: "tool_result", ToolUseID: "tool-1", ToolResult: "a deterministic tool result appended after the response anchor",
	}}}
	loop.mu.Lock()
	loop.Messages = append(loop.Messages, toolResult)
	loop.mu.Unlock()
	// The original 17-token request overhead was already included in provider
	// usage. Raising it to 23 should add only the six-token delta.
	loop.requestOverheadTokens.Store(23)
	want := base + estimateTokens([]llm.Message{toolResult}) + (23 - 17)
	if got := loop.EstimateContextTokens(); got != want {
		t.Fatalf("active context with post-response tool result = %d, want %d", got, want)
	}
}

func TestActiveContextHistoryReplacementInvalidatesSnapshot(t *testing.T) {
	provider := &activeContextTestProvider{name: "context-test", model: "context-model"}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "", 2)
	loop.Model = provider.ModelID()
	primeActiveContextSnapshot(t, loop, &usageTotals{in: 10_000, out: 100}, 0)

	replacement := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "fresh history"}}}}
	loop.Restore(replacement)
	want := estimateTokens(replacement)
	if got := loop.EstimateContextTokens(); got != want {
		t.Fatalf("estimate after history replacement = %d, want fallback %d", got, want)
	}
}

func TestActiveContextRejectsImpossibleProviderUsage(t *testing.T) {
	provider := &activeContextTestProvider{name: "context-test", model: "context-model"}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "", 2)
	loop.Model = provider.ModelID()
	primeActiveContextSnapshot(t, loop, &usageTotals{in: 90_000, cacheRead: 90_000, out: 10_000}, 0)

	want := estimateTokens(loop.History())
	if got := loop.EstimateContextTokens(); got != want {
		t.Fatalf("impossible provider usage was trusted: got %d, want fallback %d", got, want)
	}
}

func TestActiveContextRejectsUnderreportedProviderUsage(t *testing.T) {
	provider := &activeContextTestProvider{name: "context-test", model: "context-model"}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "", 2)
	loop.Model = provider.ModelID()
	loop.mu.Lock()
	loop.Messages = []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Type: "text", Text: strings.Repeat("provider omitted this input ", 200),
		}},
	}}
	req := llm.Request{
		Model: loop.Model, System: strings.Repeat("x", 160),
		Messages: append([]llm.Message(nil), loop.Messages...),
	}
	anchor := loop.contextRequestAnchorLocked(loop.Provider, req)
	loop.Messages = append(loop.Messages, llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{{Type: "text", Text: "answer"}},
	})
	loop.storeActiveContextSnapshotLocked(&usageTotals{in: 5, out: 1}, anchor)
	want := estimateTokens(loop.Messages) + 40
	loop.mu.Unlock()

	if got := loop.EstimateContextTokens(); got != want {
		t.Fatalf("underreported provider usage was trusted: got %d, want fallback %d", got, want)
	}
}

func TestActiveContextTrustsProviderUsageForNativeVisionPayload(t *testing.T) {
	provider := &activeContextTestProvider{name: "context-test", model: "context-model"}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "", 2)
	loop.Model = provider.ModelID()
	loop.mu.Lock()
	loop.Messages = []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Type: "text", Text: "describe this image"},
			{Type: "image", MediaType: "image/png", Data: strings.Repeat("A", 800_000)},
		},
	}}
	req := llm.Request{Model: loop.Model, Messages: append([]llm.Message(nil), loop.Messages...)}
	anchor := loop.contextRequestAnchorLocked(loop.Provider, req)
	loop.Messages = append(loop.Messages, llm.Message{
		Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "an image"}},
	})
	loop.storeActiveContextSnapshotLocked(&usageTotals{in: 1_200, out: 20}, anchor)
	loop.mu.Unlock()

	if local := estimateTokens(loop.History()); local < 500_000 {
		t.Fatalf("test fixture did not produce a large local base64 estimate: %d", local)
	}
	if got := loop.EstimateContextTokens(); got != 1_220 {
		t.Fatalf("native vision usage was replaced by base64 estimate: got %d, want 1220", got)
	}
}

func TestContextFallbackUsesNativeVisionAllowanceInsteadOfBase64Tokens(t *testing.T) {
	provider := &activeContextTestProvider{name: "context-test", model: "context-model"}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "", 2)
	loop.Model = provider.ModelID()
	loop.Messages = []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Type: "text", Text: "inspect"},
			{Type: "image", MediaType: "image/png", Data: strings.Repeat("B", 800_000)},
		},
	}}

	want := estimateActiveHistoryTokens(loop.History())
	if pressure := loop.EstimateRequestContextTokens(nil); pressure != want {
		t.Fatalf("request pressure=%d, want native vision estimate %d", pressure, want)
	}
	if got := loop.EstimateContextTokens(); got != want {
		t.Fatalf("active fallback=%d, want native vision estimate %d", got, want)
	}
	if want > 5_000 {
		t.Fatalf("base64 payload leaked into active meter: %d", want)
	}
}

type activeContextPolicyProvider struct {
	*activeContextTestProvider
}

func (activeContextPolicyProvider) ContextIncludesAssistantBlock(block llm.ContentBlock) bool {
	return block.Type != "thinking" && block.Type != "redacted_thinking"
}

func TestActiveContextExcludesNonReplayableReasoningOutput(t *testing.T) {
	base := &activeContextTestProvider{name: "responses", model: "reasoning-model"}
	provider := activeContextPolicyProvider{activeContextTestProvider: base}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "", 2)
	loop.Model = provider.ModelID()
	loop.mu.Lock()
	loop.Messages = []llm.Message{{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "solve it"}},
	}}
	req := llm.Request{Model: loop.Model, Messages: append([]llm.Message(nil), loop.Messages...)}
	anchor := loop.contextRequestAnchorLocked(loop.Provider, req)
	assistant := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			{Type: "thinking", Text: strings.Repeat("hidden reasoning ", 20_000)},
			{Type: "text", Text: "final answer"},
		},
	}
	loop.Messages = append(loop.Messages, assistant)
	loop.storeActiveContextSnapshotLocked(&usageTotals{in: 1_000, out: 50_000}, anchor)
	loop.mu.Unlock()

	replayed := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "final answer"}}}
	want := 1_000 + estimateTokens([]llm.Message{replayed})
	if got := loop.EstimateContextTokens(); got != want {
		t.Fatalf("non-replayed reasoning counted in active context: got %d, want %d", got, want)
	}
}

type activeContextValueProvider struct {
	*activeContextTestProvider
}

func TestActiveContextRoutingRevisionRejectsSameIdentityValueRebind(t *testing.T) {
	oldProvider := activeContextValueProvider{&activeContextTestProvider{name: "same", model: "same-model"}}
	loop := NewLoop(oldProvider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "", 2)
	loop.Model = oldProvider.ModelID()
	loop.mu.Lock()
	loop.Messages = []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "old request"}}}}
	req := llm.Request{Model: loop.Model, Messages: append([]llm.Message(nil), loop.Messages...)}
	anchor := loop.contextRequestAnchorLocked(loop.Provider, req)
	loop.mu.Unlock()

	newProvider := activeContextValueProvider{&activeContextTestProvider{name: "same", model: "same-model"}}
	loop.RebindProviderModel(newProvider, newProvider.ModelID())
	loop.mu.Lock()
	loop.Messages = append(loop.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "stale response"}}})
	loop.storeActiveContextSnapshotLocked(&usageTotals{in: 50_000, out: 1_000}, anchor)
	loop.mu.Unlock()

	want := estimateTokens(loop.History())
	if got := loop.EstimateContextTokens(); got != want {
		t.Fatalf("stale same-identity usage survived routing rebind: got %d, want fallback %d", got, want)
	}
}

func TestActiveContextKeepsProviderUsageAcrossNormalEstimatorDrift(t *testing.T) {
	provider := &activeContextTestProvider{name: "context-test", model: "context-model"}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "", 2)
	loop.Model = provider.ModelID()
	loop.mu.Lock()
	loop.Messages = []llm.Message{{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: strings.Repeat("界", 1_000)}},
	}}
	req := llm.Request{Model: loop.Model, Messages: append([]llm.Message(nil), loop.Messages...)}
	anchor := loop.contextRequestAnchorLocked(loop.Provider, req)
	loop.Messages = append(loop.Messages, llm.Message{
		Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "answer"}},
	})
	estimated := estimateTokens(loop.Messages)
	reported := estimated - 100
	loop.storeActiveContextSnapshotLocked(&usageTotals{in: reported - 10, out: 10}, anchor)
	loop.mu.Unlock()

	if got := loop.EstimateContextTokens(); got != reported {
		t.Fatalf("normal tokenizer drift discarded provider usage: got %d, want raw %d", got, reported)
	}
}

func TestActiveContextEstimatesAssistantWhenProviderOmitsOutputUsage(t *testing.T) {
	provider := &activeContextTestProvider{name: "context-test", model: "context-model"}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "", 2)
	loop.Model = provider.ModelID()
	loop.mu.Lock()
	loop.Messages = []llm.Message{{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "question"}},
	}}
	req := llm.Request{Model: loop.Model, Messages: append([]llm.Message(nil), loop.Messages...)}
	anchor := loop.contextRequestAnchorLocked(loop.Provider, req)
	assistant := llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{{Type: "text", Text: strings.Repeat("answer ", 80)}},
	}
	loop.Messages = append(loop.Messages, assistant)
	loop.storeActiveContextSnapshotLocked(&usageTotals{in: 1_000}, anchor)
	loop.mu.Unlock()

	want := 1_000 + estimateTokens([]llm.Message{assistant})
	if got := loop.EstimateContextTokens(); got != want {
		t.Fatalf("missing output usage dropped assistant: got %d, want %d", got, want)
	}
}

func TestActiveContextProviderAndModelSwitchInvalidateSnapshot(t *testing.T) {
	newLoop := func() (*Loop, *activeContextTestProvider) {
		provider := &activeContextTestProvider{name: "context-test", model: "context-model"}
		loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "", 2)
		loop.Model = provider.ModelID()
		primeActiveContextSnapshot(t, loop, &usageTotals{in: 10_000, out: 100}, 0)
		return loop, provider
	}

	t.Run("provider instance", func(t *testing.T) {
		loop, oldProvider := newLoop()
		loop.Provider = &activeContextTestProvider{name: oldProvider.name, model: oldProvider.model}
		want := estimateTokens(loop.History())
		if got := loop.EstimateContextTokens(); got != want {
			t.Fatalf("estimate after provider switch = %d, want fallback %d", got, want)
		}
	})

	t.Run("header model", func(t *testing.T) {
		loop, _ := newLoop()
		loop.Model = "different-model"
		want := estimateTokens(loop.History())
		if got := loop.EstimateContextTokens(); got != want {
			t.Fatalf("estimate after model switch = %d, want fallback %d", got, want)
		}
	})
}

func TestRebindProviderModelAtomicallyRefreshesContextAndCompactor(t *testing.T) {
	oldProvider := &activeContextTestProvider{name: "old", model: "old-model"}
	loop := NewLoop(oldProvider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "", 2)
	loop.Model = oldProvider.ModelID()
	loop.ContextWindow = oldProvider.MaxContextTokens()
	cfg := DefaultCompactionConfig()
	loop.Compactor = NewCompactor(cfg, loop.Model, loop.ContextWindow, oldProvider)
	loop.Compactor.MaxOutputTokens = 4_000
	primeActiveContextSnapshot(t, loop, &usageTotals{in: 10_000, out: 100}, 0)

	newProvider := &activeContextTestProvider{name: "new", model: "new-model"}
	loop.RebindProviderModel(newProvider, newProvider.ModelID())
	provider, model, window := loop.ProviderModelSnapshot()
	if provider != newProvider || model != newProvider.ModelID() || window != newProvider.MaxContextTokens() {
		t.Fatalf("routing tuple not rebound: provider=%T model=%q window=%d", provider, model, window)
	}
	if loop.Compactor == nil || loop.Compactor.Provider != newProvider || loop.Compactor.Model != newProvider.ModelID() {
		t.Fatalf("compactor not rebound: %+v", loop.Compactor)
	}
	if got, want := loop.EstimateContextTokens(), estimateTokens(loop.History()); got != want {
		t.Fatalf("old provider snapshot survived rebind: got %d, want fallback %d", got, want)
	}
}
