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
	if len(got) != 9 {
		t.Errorf("main agent with all conditions met should produce 9 sections, got %d", len(got))
	}
	wantOrder := []string{
		"identity", "language", "privacy", "style", "tool_redirects",
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
	// 2026-05-21 — tool_redirects is now included for sub-agents that
	// have Read/LS in their enabled set (image #31 repro: a Read-only
	// sub-agent kept calling LS on file paths because the redirects
	// table was Bash-gated and hidden from it). The table now includes
	// the directory-vs-file decision rules which apply even without Bash.
	wantNames := map[string]bool{
		"identity":       true,
		"language":       true,
		"privacy":        true,
		"style":          true,
		"tool_redirects": true,
	}
	if len(got) != len(wantNames) {
		t.Errorf("sub-agent should produce %d sections, got %d: %v",
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
	if strings.Count(s, "# Language") != 1 {
		t.Error("language directive should be assembled exactly once")
	}
}

func TestWorkingEfficientlyDefinesAutomaticExecutionStrategies(t *testing.T) {
	s := AssembleBaseString(PromptCtx{
		EnabledTools: map[string]bool{"Agent": true, "TaskCreate": true, "TodoWrite": true},
	})
	for _, want := range []string{
		"Direct execution",
		"Planned single-agent",
		"Parallel sub-agents",
		"Coordinated agent team",
		"Choose the lightest strategy",
		"Explicit user orchestration instructions override automatic strategy selection",
		"exactly N sub-agents",
		"Never silently downgrade",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("automatic execution policy is missing %q", want)
		}
	}
}

func TestAssembleMinimalSubAgentPrompt_OmitsMainOnlySections(t *testing.T) {
	_, sections := AssembleMinimalSubAgentPrompt(PromptCtx{
		Model:                "test-model",
		ProviderName:         "test-provider",
		EnabledTools:         map[string]bool{"Read": true, "Grep": true, "mcp__computer-use__screenshot": true},
		HasSkills:            true,
		ComputerUseAvailable: true,
	})

	got := make(map[string]bool, len(sections))
	for _, section := range sections {
		got[section.Name] = true
	}
	for _, name := range []string{"working_efficiently", "skills", "reversibility", "interaction_modes", "project_context", "addendum"} {
		if got[name] {
			t.Errorf("minimal sub-agent prompt should omit %q", name)
		}
	}
	for _, name := range []string{"identity", "language", "privacy", "style", "computer_use", "env"} {
		if !got[name] {
			t.Errorf("minimal sub-agent prompt should include %q; got %v", name, sectionNames(sections))
		}
	}
}

func TestAssembleMinimalSubAgentPromptUsesExplicitWorkingDirectory(t *testing.T) {
	workspace := t.TempDir()
	system, _ := AssembleMinimalSubAgentPrompt(PromptCtx{
		Model:            "model",
		WorkingDirectory: workspace,
		EnabledTools:     map[string]bool{"Read": true},
	})
	if !strings.Contains(system, "Working directory: "+workspace) {
		t.Fatalf("minimal prompt did not use rebound workspace %q:\n%s", workspace, system)
	}
}

func TestRenderBasePrompt_StillEmbedsProviderHint(t *testing.T) {
	out := RenderBasePrompt(BasePromptVars{
		Model:        "MiniMax-M2.7",
		ProviderHint: "# Provider notes (test)\nQuirk: x.",
	})
	if strings.Contains(out, "powered by MiniMax-M2.7") {
		t.Error("rendered prompt should not surface model in identity")
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
