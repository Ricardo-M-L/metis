package skills

import (
	"strings"
	"testing"
)

// TestEmbedded_AllParse — every bundled SKILL.md must parse without error.
// The embedded.go scan loop swallows errors silently to keep startup
// resilient; this test catches authoring mistakes at build time instead.
func TestEmbedded_AllParse(t *testing.T) {
	layer := bundledLayer()
	skills, err := layer.Scan()
	if err != nil {
		t.Fatalf("bundled scan: %v", err)
	}
	if len(skills) < 20 {
		t.Errorf("expected at least 20 bundled skills; got %d", len(skills))
	}
	for _, sk := range skills {
		if sk.Name == "" {
			t.Errorf("skill missing name: %+v", sk)
		}
		if sk.Description == "" {
			t.Errorf("skill %q missing description", sk.Name)
		}
		if len(strings.TrimSpace(sk.Prompt)) < 50 {
			t.Errorf("skill %q has too-short prompt body (%d chars)", sk.Name, len(sk.Prompt))
		}
	}
}

func TestEmbedded_NoDuplicateNames(t *testing.T) {
	layer := bundledLayer()
	skills, _ := layer.Scan()
	seen := map[string]bool{}
	for _, sk := range skills {
		if seen[sk.Name] {
			t.Errorf("duplicate skill name: %q", sk.Name)
		}
		seen[sk.Name] = true
	}
}

func TestEmbedded_AllowedToolsKnown(t *testing.T) {
	// Known tool names from internal/tools/builtin/register.go. If a skill
	// references something not in this list, it's almost certainly a typo
	// (e.g. "Reed" for "Read") that would silently fail at runtime.
	known := map[string]bool{
		"Read": true, "Write": true, "Edit": true, "Bash": true,
		"LS": true, "Glob": true, "Grep": true, "WebFetch": true,
		"Git": true, "Search": true, "TodoWrite": true, "TodoRead": true,
		"AskUser": true, "Skill": true, "Memory": true,
		"Agent": true, "SendMessage": true,
	}
	layer := bundledLayer()
	skills, _ := layer.Scan()
	for _, sk := range skills {
		for _, tool := range sk.AllowedTools {
			if !known[tool] {
				t.Errorf("skill %q references unknown tool %q (likely typo)", sk.Name, tool)
			}
		}
	}
}

func TestEmbedded_RequiredFields(t *testing.T) {
	// Every bundled skill must have name + description + when_to_use +
	// version. This guard prevents shipping a half-written skill.
	layer := bundledLayer()
	skills, _ := layer.Scan()
	for _, sk := range skills {
		if sk.Name == "" {
			t.Errorf("missing name: %+v", sk)
		}
		if sk.Description == "" {
			t.Errorf("skill %q missing description", sk.Name)
		}
		if sk.WhenToUse == "" {
			t.Errorf("skill %q missing when_to_use (helps the LLM auto-pick)", sk.Name)
		}
		if sk.Version == "" {
			t.Errorf("skill %q missing version", sk.Name)
		}
	}
}
