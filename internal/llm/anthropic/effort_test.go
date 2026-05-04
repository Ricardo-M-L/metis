package anthropic

// Effort propagation tests. Pin the contract: Effort + MaxTokens
// override on Request must flow through to the Anthropic wire-format
// body. Safety net for the /effort and /fast slash commands.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToAnthropic_EffortHighSetsThinkingBudget(t *testing.T) {
	req := Request{
		Model:  "claude-opus-4-7",
		System: "you are a tester",
		Effort: EffortHigh,
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
