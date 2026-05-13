package runtime

import (
	"strings"
	"testing"
)

func TestDetectProviderCapabilities_Anthropic(t *testing.T) {
	c := DetectProviderCapabilities("anthropic", "claude-opus-4-7")
	if !c.XMLPreferred || !c.AnthropicCompatTools || !c.HasThinkingBlocks || !c.PromptCachingActive {
		t.Errorf("Anthropic should have XML+tools+thinking+cache; got %+v", c)
	}
}

func TestDetectProviderCapabilities_MiniMaxFlagsBug(t *testing.T) {
	c := DetectProviderCapabilities("minimax", "MiniMax-M2.7")
	if !c.EmptyArgsToolUseBug {
		t.Errorf("MiniMax must flag EmptyArgsToolUseBug; got %+v", c)
	}
	if !c.AnthropicCompatTools {
		t.Errorf("metis routes MiniMax via Anthropic endpoint; should flag AnthropicCompatTools")
	}
}

func TestDetectProviderCapabilities_KimiThinkingVariant(t *testing.T) {
	base := DetectProviderCapabilities("kimi", "kimi-k2.6")
	think := DetectProviderCapabilities("kimi", "kimi-k2-thinking-turbo")
	if base.HasThinkingBlocks {
		t.Error("plain kimi should not flag thinking blocks")
	}
	if !think.HasThinkingBlocks {
		t.Error("kimi thinking variant should flag thinking blocks")
	}
}

func TestDetectProviderCapabilities_CustomUnwrap(t *testing.T) {
	c := DetectProviderCapabilities("custom:kimi", "kimi-k2.6")
	if c.FamilyName != "Moonshot Kimi" {
		t.Errorf("custom:kimi should unwrap to Kimi; got %q", c.FamilyName)
	}
}

func TestDetectProviderCapabilities_UnknownYieldsEmpty(t *testing.T) {
	c := DetectProviderCapabilities("fictional-provider", "x")
	if c.FamilyName != "" {
		t.Errorf("unknown provider should yield empty caps; got %+v", c)
	}
}

func TestComposeProviderHint_EmptyCapsEmitsNothing(t *testing.T) {
	if got := composeProviderHint(ProviderCapabilities{}); got != "" {
		t.Errorf("empty caps should compose to empty; got %q", got)
	}
}

func TestComposeProviderHint_AtomsAppearWhenFlagsSet(t *testing.T) {
	c := ProviderCapabilities{
		FamilyName:           "Test",
		AnthropicCompatTools: true,
		XMLPreferred:         true,
		EmptyArgsToolUseBug:  true,
		HasThinkingBlocks:    true,
		LongContext:          true,
		Bilingual:            true,
		VerboseByDefault:     true,
	}
	got := composeProviderHint(c)
	atoms := []string{
		"Test",                       // header
		"Anthropic-compatible",       // anthropic shape
		"empty input arguments",      // empty-args bug
		"dedicated thinking blocks",  // thinking
		"≥200K",                      // long context
		"Chinese or English",         // bilingual
		"over-explain by default",    // verbose
	}
	for _, a := range atoms {
		if !strings.Contains(got, a) {
			t.Errorf("composed hint missing %q; got:\n%s", a, got)
		}
	}
}

func TestComposeProviderHint_OnlyOneOfThinkingOrReasoningSplit(t *testing.T) {
	c := ProviderCapabilities{
		FamilyName:        "X",
		HasThinkingBlocks: true,
		HasReasoningSplit: true,
	}
	got := composeProviderHint(c)
	// Should include thinking-blocks atom and NOT reasoning-split atom
	// (the switch picks thinking first since wire shape is different
	// but the user-facing advice is the same — emitting both is noise).
	if !strings.Contains(got, "dedicated thinking blocks") {
		t.Errorf("expected thinking-blocks atom; got:\n%s", got)
	}
	if strings.Contains(got, "reasoning_content") {
		t.Errorf("should not also emit reasoning_content atom when thinking already covers the rule; got:\n%s", got)
	}
}

func TestComposeProviderHint_OpenAIGetsFunctionCallAtom(t *testing.T) {
	got := ProviderHintFor("openai", "gpt-4o")
	if !strings.Contains(got, "function-call") && !strings.Contains(got, "function-calling") {
		t.Errorf("OpenAI hint should mention function-call shape; got:\n%s", got)
	}
}
