package runtime

import (
	"strings"
	"testing"
)

func TestProviderHintFor_DispatchesByProvider(t *testing.T) {
	cases := []struct {
		name       string
		provider   string
		model      string
		mustContain string
	}{
		{"anthropic", "anthropic", "claude-opus-4-7", "Anthropic"},
		{"minimax", "minimax", "MiniMax-M2.7", "MiniMax"},
		{"deepseek", "deepseek", "deepseek-v4-pro", "DeepSeek"},
		{"kimi base", "kimi", "kimi-k2.6", "Moonshot"},
		{"openai", "openai", "gpt-4o", "OpenAI"},
		{"gemini", "gemini", "gemini-3.1-flash-lite-preview", "Gemini"},
		{"glm", "glm", "glm-5.1", "GLM"},
		{"unknown empty", "fictional-provider", "x", ""},
		{"case insensitive", "ANTHROPIC", "x", "Anthropic"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ProviderHintFor(c.provider, c.model)
			if c.mustContain == "" {
				if got != "" {
					t.Errorf("expected empty hint, got %q", got)
				}
				return
			}
			if !strings.Contains(got, c.mustContain) {
				t.Errorf("hint for %s/%s missing %q; got:\n%s", c.provider, c.model, c.mustContain, got)
			}
		})
	}
}

func TestProviderHintFor_KimiThinkingAppendsExtra(t *testing.T) {
	base := ProviderHintFor("kimi", "kimi-k2")
	withThink := ProviderHintFor("kimi", "kimi-k2-thinking-turbo")
	if len(withThink) <= len(base) {
		t.Errorf("kimi-k2-thinking variant should be longer than base; base=%d thinking=%d", len(base), len(withThink))
	}
	if !strings.Contains(withThink, "thinking") {
		t.Errorf("kimi thinking hint missing thinking guidance:\n%s", withThink)
	}
}

func TestProviderHintFor_CustomWrapperResolves(t *testing.T) {
	got := ProviderHintFor("custom:kimi", "kimi-k2.6")
	if !strings.Contains(got, "Moonshot") {
		t.Errorf("custom:kimi should resolve to kimi hint; got %q", got)
	}
}

func TestRenderBasePrompt_EmbedsProviderHint(t *testing.T) {
	out := RenderBasePrompt(BasePromptVars{
		Model:        "MiniMax-M2.7",
		ProviderHint: ProviderHintFor("minimax", "MiniMax-M2.7"),
	})
	if !strings.Contains(out, "MiniMax-M2.7") {
		t.Errorf("rendered prompt missing model name; got:\n%s", out)
	}
	if !strings.Contains(out, "empty input arguments") {
		t.Errorf("rendered prompt missing minimax-specific quirk; got:\n%s", out)
	}
}

func TestRenderBasePrompt_NoHintWhenEmpty(t *testing.T) {
	out := RenderBasePrompt(BasePromptVars{Model: "x"})
	if strings.Contains(out, "# Provider notes") {
		t.Errorf("expected no provider notes section when ProviderHint empty:\n%s", out)
	}
}
