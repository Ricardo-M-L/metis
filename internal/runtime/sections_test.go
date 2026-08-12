package runtime

import (
	"strings"
	"testing"
)

func TestIdentitySection_DoesNotExpandModel(t *testing.T) {
	got := IdentitySection(PromptCtx{Model: "claude-opus-4-7"})
	if got.Name != "identity" {
		t.Errorf("Name=%q, want identity", got.Name)
	}
	if strings.Contains(got.Body, "powered by") || strings.Contains(got.Body, "claude-opus-4-7") {
		t.Errorf("identity body should not surface model name; got:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, "You are metis, a fast, local-first agent CLI.") {
		t.Errorf("identity body missing metis intro; got:\n%s", got.Body)
	}
	if !got.Cache {
		t.Error("identity should be cacheable")
	}
}

func TestIdentitySection_NoModelStillOmitsClause(t *testing.T) {
	got := IdentitySection(PromptCtx{})
	if strings.Contains(got.Body, "powered by") {
		t.Errorf("empty model should not render 'powered by' clause; got:\n%s", got.Body)
	}
}

func TestToolRedirectsSection_FiresForBashOrReadOrLS(t *testing.T) {
	// EnabledTools nil → legacy "assume all" → section present.
	if got := ToolRedirectsSection(PromptCtx{}); got.Name == "" {
		t.Error("legacy nil EnabledTools should fire the section")
	}
	// Bash present → fires (always has).
	if got := ToolRedirectsSection(PromptCtx{EnabledTools: map[string]bool{"Bash": true}}); got.Name == "" {
		t.Error("Bash present should fire tool_redirects")
	}
	// 2026-05-21 — Read OR LS alone now fires too. The redirects
	// table includes the directory-vs-file decision rules which
	// apply without Bash; previously gated only on Bash, which left
	// LS+Read sub-agents (image #31) without guidance.
	if got := ToolRedirectsSection(PromptCtx{EnabledTools: map[string]bool{"Read": true, "Grep": true}}); got.Name == "" {
		t.Error("Read-only set should still fire tool_redirects (for LS↔Read guidance)")
	}
	if got := ToolRedirectsSection(PromptCtx{EnabledTools: map[string]bool{"LS": true, "Grep": true}}); got.Name == "" {
		t.Error("LS-only set should fire tool_redirects")
	}
	// Truly Bash/Read/LS-less set → section omitted (e.g. a memory-
	// only sub-agent that only has Memory + MetisInfo).
	if got := ToolRedirectsSection(PromptCtx{EnabledTools: map[string]bool{"Grep": true, "Memory": true}}); got.Name != "" {
		t.Errorf("set with no Bash/Read/LS should omit tool_redirects; got Name=%q", got.Name)
	}
}

func TestWorkingEfficientlySection_SubAgentSkipped(t *testing.T) {
	if got := WorkingEfficientlySection(PromptCtx{IsSubAgent: true}); got.Name != "" {
		t.Errorf("sub-agent should skip working_efficiently; got %q", got.Name)
	}
	if got := WorkingEfficientlySection(PromptCtx{}); got.Name == "" {
		t.Error("main agent should include working_efficiently")
	}
}

func TestSkillsSection_RequiresSkills(t *testing.T) {
	if got := SkillsSection(PromptCtx{HasSkills: false}); got.Name != "" {
		t.Errorf("no skills installed should omit section; got %q", got.Name)
	}
	if got := SkillsSection(PromptCtx{HasSkills: true}); got.Name == "" {
		t.Error("HasSkills=true should fire skills section")
	}
	if got := SkillsSection(PromptCtx{HasSkills: true, IsSubAgent: true}); got.Name != "" {
		t.Errorf("sub-agent should skip skills even when installed; got %q", got.Name)
	}
}

func TestReversibilitySection_SubAgentSkipped(t *testing.T) {
	if got := ReversibilitySection(PromptCtx{IsSubAgent: true}); got.Name != "" {
		t.Errorf("sub-agent should skip reversibility; got %q", got.Name)
	}
	main := ReversibilitySection(PromptCtx{})
	if main.Name == "" {
		t.Error("main agent should include reversibility")
	}
	if !strings.Contains(main.Body, "rm -rf") {
		t.Errorf("reversibility body should warn about rm -rf; got:\n%s", main.Body)
	}
}

func TestDefaultSectionGetters_Ordered(t *testing.T) {
	getters := DefaultSectionGetters()
	wantNames := []string{
		"identity", "language", "computer_use", "privacy", "style", "tool_redirects",
		"working_efficiently", "skills", "reversibility",
		"interaction_modes",
	}
	if len(getters) != len(wantNames) {
		t.Fatalf("expected %d getters, got %d", len(wantNames), len(getters))
	}
	// Drive them with a maximal ctx that fires every section, including
	// a mcp__computer-use__* tool name so ComputerUseSection lights up.
	ctx := PromptCtx{
		Model:        "x",
		EnabledTools: map[string]bool{"Bash": true, "mcp__computer-use__screenshot": true},
		HasSkills:    true,
	}
	for i, getter := range getters {
		sec := getter(ctx)
		if sec.Name != wantNames[i] {
			t.Errorf("getter[%d] produced %q, want %q", i, sec.Name, wantNames[i])
		}
	}
}

func TestDefaultSectionGetters_SubAgentConfigShrinksPrompt(t *testing.T) {
	full := assembleNames(PromptCtx{
		EnabledTools: map[string]bool{"Bash": true},
		HasSkills:    true,
	})
	sub := assembleNames(PromptCtx{
		EnabledTools: map[string]bool{"Read": true, "Grep": true}, // no Bash
		HasSkills:    true,
		IsSubAgent:   true,
	})
	if len(full) <= len(sub) {
		t.Errorf("sub-agent prompt should have fewer sections than main; full=%v sub=%v", full, sub)
	}
	// Specifically these should be gone for sub-agent:
	// 2026-05-21 — tool_redirects removed from the omit-list because
	// the LS↔Read directory-vs-file rules in that table apply even
	// without Bash (image #31 repro). working_efficiently / skills /
	// reversibility remain main-only.
	mustOmit := []string{"working_efficiently", "skills", "reversibility"}
	subSet := map[string]bool{}
	for _, n := range sub {
		subSet[n] = true
	}
	for _, n := range mustOmit {
		if subSet[n] {
			t.Errorf("sub-agent prompt should omit %q; got sections=%v", n, sub)
		}
	}
}

// assembleNames runs every default getter and returns the names of the
// sections that fired (skipped sections have Name=="").
func assembleNames(ctx PromptCtx) []string {
	var names []string
	for _, g := range DefaultSectionGetters() {
		s := g(ctx)
		if s.Name != "" {
			names = append(names, s.Name)
		}
	}
	return names
}
