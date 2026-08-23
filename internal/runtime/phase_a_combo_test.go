package runtime

import (
	"strings"
	"testing"
)

// TestRenderBasePrompt_WithModel verifies the model is not surfaced
// in the identity text even when provided.
func TestRenderBasePrompt_WithModel(t *testing.T) {
	out := RenderBasePrompt(BasePromptVars{Model: "claude-opus-4-7"})
	if strings.Contains(out, "powered by claude-opus-4-7") {
		t.Errorf("base prompt should not mention model in identity; got:\n%s", out)
	}
	if !strings.Contains(out, "You are metis, a fast, local-first agent.") {
		t.Errorf("base prompt missing metis identity line; got:\n%s", out)
	}
	// Legacy / empty variant must not contain the conditional clause.
	out2 := RenderBasePrompt(BasePromptVars{})
	if strings.Contains(out2, "powered by") {
		t.Errorf("empty Model must omit 'powered by'; got:\n%s", out2)
	}
}

// TestRenderBasePrompt_WithProviderHint verifies the ProviderHint
// trailing block expansion.
func TestRenderBasePrompt_WithProviderHint(t *testing.T) {
	hint := "When in doubt, wrap arguments in XML tags."
	out := RenderBasePrompt(BasePromptVars{ProviderHint: hint})
	if !strings.Contains(out, hint) {
		t.Errorf("ProviderHint should appear verbatim; got:\n%s", out)
	}
}

// TestAssembleSystemPromptSections_FullMode produces sections in
// expected order with cache flags.
func TestAssembleSystemPromptSections_FullMode(t *testing.T) {
	secs := AssembleSystemPromptSections("BASE_TEST", AssembleOptions{Mode: PromptFull})
	// Always: base + env. project_context / addendum depend on cwd
	// state, so we just ensure the first two are correctly named/cached.
	if len(secs) < 2 {
		t.Fatalf("expected at least 2 sections, got %d", len(secs))
	}
	if secs[0].Name != "base" || secs[0].Body != "BASE_TEST" {
		t.Errorf("section[0] should be base/BASE_TEST, got %+v", secs[0])
	}
	if !secs[0].Cache {
		t.Errorf("base section must be cacheable")
	}
	if secs[1].Name != "env" {
		t.Errorf("section[1] should be env, got %s", secs[1].Name)
	}
	if secs[1].Cache {
		t.Errorf("env section must NOT be cacheable (cwd/date change)")
	}
}

// TestAssembleSystemPromptSections_MinimalMode has only base + env.
func TestAssembleSystemPromptSections_MinimalMode(t *testing.T) {
	secs := AssembleSystemPromptSections("X", AssembleOptions{Mode: PromptMinimal})
	for _, s := range secs {
		if s.Name == "project_context" || s.Name == "addendum" {
			t.Errorf("minimal mode must skip %q section", s.Name)
		}
	}
}

// TestAssembleSystemPromptSections_MediumMode keeps project_context but
// drops addendum.
func TestAssembleSystemPromptSections_MediumMode(t *testing.T) {
	secs := AssembleSystemPromptSections("X", AssembleOptions{Mode: PromptMedium})
	for _, s := range secs {
		if s.Name == "addendum" {
			t.Errorf("medium mode must skip addendum; got: %+v", s)
		}
	}
}

// TestAssembleSystemPromptSections_WithOverlay inserts overlays
// between base and env.
func TestAssembleSystemPromptSections_WithOverlay(t *testing.T) {
	overlay := SystemPromptSection{Name: "provider:claude", Body: "I am Claude.", Cache: true}
	secs := AssembleSystemPromptSections("BASE", AssembleOptions{
		Mode:     PromptMinimal,
		Overlays: []SystemPromptSection{overlay},
	})
	// Expected: [base, provider:claude, env]
	if len(secs) < 3 {
		t.Fatalf("expected ≥3 sections (base, overlay, env); got %d", len(secs))
	}
	if secs[0].Name != "base" || secs[1].Name != "provider:claude" || secs[2].Name != "env" {
		t.Errorf("overlay should sit between base and env; got %s/%s/%s",
			secs[0].Name, secs[1].Name, secs[2].Name)
	}
}

// TestRenderSections_LegacyStringFormPreservesBoundary verifies that
// the legacy string-form rendering keeps the cache-boundary markers
// in place so the anthropic provider's existing buildSystemBlocks
// (which parses markers) still produces a cached static prefix.
func TestRenderSections_LegacyStringFormPreservesBoundary(t *testing.T) {
	secs := []SystemPromptSection{
		{Name: "base", Body: "STATIC", Cache: true},
		{Name: "env", Body: "<env>cwd=/tmp</env>", Cache: false},
		{Name: "addendum", Body: "USER_NOTE", Cache: true},
	}
	rendered := RenderSections(secs)
	if !strings.Contains(rendered, "STATIC") {
		t.Errorf("rendered prompt missing base body")
	}
	if !strings.Contains(rendered, "<env>") {
		t.Errorf("rendered prompt missing env block")
	}
	if !strings.Contains(rendered, "USER_NOTE") {
		t.Errorf("rendered prompt missing addendum")
	}
	// At least one boundary marker should appear (between cached
	// and non-cached, or boundary 2 between two cached sections).
	if !strings.Contains(rendered, "__METIS_CACHE_BOUNDARY") {
		t.Errorf("rendered prompt should contain a cache-boundary marker; got:\n%s", rendered)
	}
}

// TestAssembleSystemPrompt_LegacyMinimalFlagStillWorks verifies the
// pre-Mode `Minimal: true` opts continue to produce minimal output.
func TestAssembleSystemPrompt_LegacyMinimalFlagStillWorks(t *testing.T) {
	out := AssembleSystemPromptWithOptions("BASE", AssembleOptions{Minimal: true})
	// Should NOT contain a project_context block (PromptMinimal).
	if strings.Contains(out, "<project_context") {
		t.Errorf("legacy Minimal=true should drop <project_context>; got:\n%s", out)
	}
}
