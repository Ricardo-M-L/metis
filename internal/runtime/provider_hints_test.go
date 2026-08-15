package runtime

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestProviderHintFor_DispatchesByProvider(t *testing.T) {
	cases := []struct {
		name        string
		provider    string
		model       string
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
	if strings.Contains(out, "powered by MiniMax-M2.7") {
		t.Errorf("rendered prompt should not surface model name in identity; got:\n%s", out)
	}
	if !strings.Contains(out, "# Provider notes (MiniMax M-series)") {
		t.Errorf("rendered prompt missing provider hint section; got:\n%s", out)
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

func TestRebindProviderPrompt_ReplacesManagedHintInBothForms(t *testing.T) {
	oldHint := ProviderHintFor("anthropic", "claude-sonnet-4-6")
	sections := []llm.SystemSection{
		{Name: "identity", Body: "identity", Cache: true},
		{Name: "provider_hint", Body: oldHint, Cache: true},
		{Name: "env", Body: "env", Volatile: true},
	}

	system, got := RebindProviderPrompt("stale", sections, "kimi", "kimi-k2.6")
	newHint := ProviderHintFor("kimi", "kimi-k2.6")
	if len(got) != 3 || got[1].Name != "provider_hint" || got[1].Body != newHint {
		t.Fatalf("provider hint section not replaced: %+v", got)
	}
	if strings.Contains(system, oldHint) || !strings.Contains(system, newHint) {
		t.Fatalf("legacy system form not refreshed:\n%s", system)
	}
}

func TestRebindProviderPrompt_PreservesExplicitBasePrompt(t *testing.T) {
	sections := []llm.SystemSection{{Name: "base", Body: "user supplied", Cache: true}}
	system, got := RebindProviderPrompt("user supplied", sections, "openai", "gpt-4o")
	if system != "user supplied" || len(got) != 1 || got[0] != sections[0] {
		t.Fatalf("explicit prompt changed: system=%q sections=%+v", system, got)
	}
}

// Per-model behavioral nudges (2026-06-13, point #3): each family that
// declares BehaviorNotes surfaces a labelled ## Behavior section, and
// the nudges are family-specific (Kimi → task tracking, Anthropic →
// destructive-action caution, DeepSeek → act-don't-narrate).
func TestProviderHint_BehaviorNotes(t *testing.T) {
	kimi := ProviderHintFor("kimi", "kimi-k2.6")
	if !strings.Contains(kimi, "## Behavior") {
		t.Errorf("kimi hint missing ## Behavior section; got:\n%s", kimi)
	}
	if !strings.Contains(kimi, "Todo/Task") {
		t.Errorf("kimi behavior should push task tracking; got:\n%s", kimi)
	}

	anthropic := ProviderHintFor("anthropic", "claude-opus-4-8")
	if !strings.Contains(anthropic, "reversibility") {
		t.Errorf("anthropic behavior should stress reversibility; got:\n%s", anthropic)
	}

	deepseek := ProviderHintFor("deepseek", "deepseek-v4-pro")
	if !strings.Contains(deepseek, "## Behavior") || !strings.Contains(deepseek, "narrat") {
		t.Errorf("deepseek behavior should say act-don't-narrate; got:\n%s", deepseek)
	}

	// A family without BehaviorNotes (OpenAI) must NOT emit the section.
	openai := ProviderHintFor("openai", "gpt-5.1")
	if strings.Contains(openai, "## Behavior") {
		t.Errorf("openai has no behavior notes; should not emit the section; got:\n%s", openai)
	}
}
