package runtime

import (
	"strings"
	"testing"
)

func TestRebindToolAwarePromptUsesFinalVisibleTools(t *testing.T) {
	provisional := PromptCtx{EnabledTools: nil, HasSkills: true}
	sections := AssembleSystemPromptSectionsCtx(provisional, AssembleOptions{SkipEnv: true})
	system := RenderSections(sections)
	if !strings.Contains(system, "WebSearch") {
		t.Fatal("provisional legacy prompt should contain web routing")
	}

	readOnly := provisional
	readOnly.EnabledTools = map[string]bool{"Read": true}
	readSystem, readSections := RebindToolAwarePrompt(system, sections, readOnly)
	if strings.Contains(readSystem, "WebSearch") || strings.Contains(readSystem, "webReader") {
		t.Fatalf("Read-only prompt retained unavailable web routing:\n%s", readSystem)
	}
	if !hasPromptSection(readSections, "tool_redirects") || !strings.Contains(readSystem, "`Read`") {
		t.Fatal("Read-only prompt lost the filesystem routing section")
	}

	webOnly := provisional
	webOnly.EnabledTools = map[string]bool{"WebSearch": true}
	webSystem, webSections := RebindToolAwarePrompt(system, sections, webOnly)
	if !hasPromptSection(webSections, "tool_redirects") || !strings.Contains(webSystem, "WebSearch") || !strings.Contains(webSystem, "webReader") {
		t.Fatalf("WebSearch prompt is missing web routing:\n%s", webSystem)
	}
}

func TestRebindToolAwarePromptPreservesExplicitBase(t *testing.T) {
	sections := []SystemPromptSection{{Name: "base", Body: "custom WebSearch wording", Cache: true}}
	gotSystem, gotSections := RebindToolAwarePrompt("custom WebSearch wording", sections, PromptCtx{
		EnabledTools: map[string]bool{"Read": true},
	})
	if gotSystem != "custom WebSearch wording" || len(gotSections) != 1 || gotSections[0] != sections[0] {
		t.Fatalf("explicit prompt changed: system=%q sections=%+v", gotSystem, gotSections)
	}
}

func hasPromptSection(sections []SystemPromptSection, name string) bool {
	for _, section := range sections {
		if section.Name == name {
			return true
		}
	}
	return false
}
