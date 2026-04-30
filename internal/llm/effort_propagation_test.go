package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests pin the contract: Effort + MaxTokens override on Request must
// flow through to the wire-format request body for both adapters. They're
// the safety net for the BROKEN /effort and /fast slash commands — without
// these we'd have no way to catch a regression where Loop sets req.Effort
// but the adapter silently drops it.

func TestToAnthropic_EffortHighSetsThinkingBudget(t *testing.T) {
	req := Request{
		Model:    "claude-opus-4-7",
		System:   "you are a tester",
		Effort:   EffortHigh,
	}
	body := toAnthropic(req, "claude-opus-4-7", 8192)
	if body.Thinking == nil {
		t.Fatal("EffortHigh should populate Thinking; got nil")
	}
	if body.Thinking.Type != "enabled" {
		t.Errorf("Thinking.Type = %q, want enabled", body.Thinking.Type)
	}
	if body.Thinking.BudgetTokens != 16384 {
		t.Errorf("Thinking.BudgetTokens = %d, want 16384", body.Thinking.BudgetTokens)
	}
}

func TestToAnthropic_DefaultEffortOmitsThinking(t *testing.T) {
	req := Request{Model: "x", Effort: EffortDefault}
	body := toAnthropic(req, "x", 100)
	if body.Thinking != nil {
		t.Errorf("EffortDefault should omit Thinking; got %+v", body.Thinking)
	}
	// Also: marshal and confirm the field is fully absent (omitempty).
	b, _ := json.Marshal(body)
	if strings.Contains(string(b), "thinking") {
		t.Errorf("marshal should not emit thinking key; got %s", b)
	}
}

func TestToAnthropic_RequestMaxTokensOverridesProviderDefault(t *testing.T) {
	req := Request{Model: "x", MaxTokens: 512}
	body := toAnthropic(req, "x", 9000)
	if body.MaxTokens != 512 {
		t.Errorf("MaxTokens = %d, want 512 (request override)", body.MaxTokens)
	}
}

func TestToAnthropic_NoOverrideUsesProviderDefault(t *testing.T) {
	req := Request{Model: "x"}
	body := toAnthropic(req, "x", 9000)
	if body.MaxTokens != 9000 {
		t.Errorf("MaxTokens = %d, want 9000 (provider default)", body.MaxTokens)
	}
}

func TestToOpenAI_EffortPropagates(t *testing.T) {
	req := Request{Effort: EffortMedium}
	body := toOpenAI(req, "gpt-5", 8192)
	if body.ReasoningEffort != "medium" {
		t.Errorf("ReasoningEffort = %q, want medium", body.ReasoningEffort)
	}
}

func TestToOpenAI_DefaultOmitsReasoningEffort(t *testing.T) {
	req := Request{Effort: EffortDefault}
	body := toOpenAI(req, "gpt-5", 8192)
	if body.ReasoningEffort != "" {
		t.Errorf("ReasoningEffort should be empty for default; got %q", body.ReasoningEffort)
	}
	b, _ := json.Marshal(body)
	if strings.Contains(string(b), "reasoning_effort") {
		t.Errorf("marshal should not emit reasoning_effort key; got %s", b)
	}
}

func TestToOpenAI_RequestMaxTokensOverridesProviderDefault(t *testing.T) {
	req := Request{MaxTokens: 256}
	body := toOpenAI(req, "x", 9000)
	if body.MaxTokens != 256 {
		t.Errorf("MaxTokens = %d, want 256", body.MaxTokens)
	}
}
