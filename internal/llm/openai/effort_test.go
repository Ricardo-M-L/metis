package openai

// Effort propagation tests for the OpenAI wire format. Mirror
// anthropic/effort_test.go's intent: ensure Request.Effort and
// Request.MaxTokens reach the wire body. Splits out from the
// pre-refactor cross-provider effort_propagation_test.go.

import (
	"encoding/json"
	"strings"
	"testing"
)

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
