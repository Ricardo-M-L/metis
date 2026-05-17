package runtime

import (
	"strings"
	"testing"
)

func TestIdentitySection_ExpandsModel(t *testing.T) {
	got := IdentitySection(PromptCtx{Model: "claude-opus-4-7"})
	if got.Name != "identity" {
		t.Errorf("Name=%q, want identity", got.Name)
	}
	if !strings.Contains(got.Body, "powered by claude-opus-4-7") {
		t.Errorf("identity body missing model substitution; got:\n%s", got.Body)
	}
	if !got.Cache {
		t.Error("identity should be cacheable")
	}
}

func TestIdentitySection_NoModelOmitsClause(t *testing.T) {
	got := IdentitySection(PromptCtx{})
	if strings.Contains(got.Body, "powered by") {
		t.Errorf("empty model should not render 'powered by' clause; got:\n%s", got.Body)
	}
}

func TestToolRedirectsSection_OmittedWhenBashDisabled(t *testing.T) {
	// EnabledTools nil → legacy "assume all" → section present
	got := ToolRedirectsSection(PromptCtx{})
	if got.Name == "" {
		t.Error("legacy nil EnabledTools should fire the section")
	}
	// EnabledTools set but no Bash → section omitted
	got2 := ToolRedirectsSection(PromptCtx{EnabledTools: map[string]bool{"Read": true, "Grep": true}})
	if got2.Name != "" {
		t.Errorf("Bash-less tool set should omit tool_redirects; got Name=%q", got2.Name)
	}
	// Bash present → section fires
	got3 := ToolRedirectsSection(PromptCtx{EnabledTools: map[string]bool{"Bash": true}})
	if got3.Name == "" {
		t.Error("Bash present should fire tool_redirects")
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
		"identity", "privacy", "style", "tool_redirects",
		"working_efficiently", "skills", "reversibility",
		"interaction_modes",
	}
	if len(getters) != len(wantNames) {
		t.Fatalf("expected %d getters, got %d", len(wantNames), len(getters))
	}
	// Drive them with a maximal ctx that fires every section.
	ctx := PromptCtx{
		Model:        "x",
		EnabledTools: map[string]bool{"Bash": true},
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
	mustOmit := []string{"tool_redirects", "working_efficiently", "skills", "reversibility"}
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
