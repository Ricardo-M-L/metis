package runtime

import (
	"strings"
	"testing"
)

func TestAssembleBaseSections_MainAgentFiresAll(t *testing.T) {
	got := AssembleBaseSections(PromptCtx{
		Model:        "test-model",
		EnabledTools: map[string]bool{"Bash": true},
		HasSkills:    true,
	})
	if len(got) != 8 {
		t.Errorf("main agent with all conditions met should produce 8 sections, got %d", len(got))
	}
	wantOrder := []string{
		"identity", "privacy", "style", "tool_redirects",
		"working_efficiently", "skills", "reversibility",
		"interaction_modes",
	}
	for i, sec := range got {
		if sec.Name != wantOrder[i] {
			t.Errorf("section[%d] = %q, want %q", i, sec.Name, wantOrder[i])
		}
	}
}

func TestAssembleBaseSections_SubAgentMinimal(t *testing.T) {
	got := AssembleBaseSections(PromptCtx{
		Model:        "test-model",
		EnabledTools: map[string]bool{"Read": true, "Grep": true},
		HasSkills:    true,
		IsSubAgent:   true,
	})
	wantNames := map[string]bool{"identity": true, "privacy": true, "style": true}
	if len(got) != len(wantNames) {
		t.Errorf("sub-agent should produce %d sections (identity+privacy+style), got %d: %v",
			len(wantNames), len(got), sectionNames(got))
	}
	for _, sec := range got {
		if !wantNames[sec.Name] {
			t.Errorf("unexpected section %q in sub-agent prompt", sec.Name)
		}
	}
}

func TestAssembleBaseString_NonEmpty(t *testing.T) {
	s := AssembleBaseString(PromptCtx{Model: "x", EnabledTools: map[string]bool{"Bash": true}, HasSkills: true})
	if len(s) < 1000 {
		t.Errorf("assembled string too short: %d chars", len(s))
	}
	// Sanity: section bodies must be joined with double-newlines
	if !strings.Contains(s, "\n\n") {
		t.Error("expected double-newline between sections")
	}
}

func TestRenderBasePrompt_StillEmbedsProviderHint(t *testing.T) {
	out := RenderBasePrompt(BasePromptVars{
		Model:        "MiniMax-M2.7",
		ProviderHint: "# Provider notes (test)\nQuirk: x.",
	})
	if !strings.Contains(out, "MiniMax-M2.7") {
		t.Error("rendered prompt missing model")
	}
	if !strings.Contains(out, "# Provider notes (test)") {
		t.Error("rendered prompt missing provider hint suffix")
	}
}

func sectionNames(secs []SystemPromptSection) []string {
	out := make([]string, len(secs))
	for i, s := range secs {
		out[i] = s.Name
	}
	return out
}
