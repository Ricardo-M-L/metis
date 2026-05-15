package openai

// system_sections_test.go pins the 2026-05-15 fix for OpenAI-dialect
// providers silently dropping req.SystemSections.
//
// The agent loop's buildRequest writes memory + auto-retrieve into
// SystemSections (not the legacy req.System string) because the
// Anthropic provider needs typed sections for per-section
// cache_control. OpenAI / DeepSeek / Kimi / MiniMax / GLM all share
// the openai.go serializer, which used to read only req.System and
// drop SystemSections on the floor. Net effect: AutoRetrieve and
// memory injection were no-ops on every non-Anthropic provider.
//
// flattenSystem now joins all section bodies into one system
// message so the content reaches the wire.

import (
	"strings"
	"testing"
)

func TestFlattenSystem_NoSections_ReturnsLegacyString(t *testing.T) {
	t.Parallel()
	got := flattenSystem(Request{System: "you are a tester"})
	if got != "you are a tester" {
		t.Errorf("with no sections, should return req.System verbatim; got %q", got)
	}
}

func TestFlattenSystem_SectionsJoinedWithDoubleNewline(t *testing.T) {
	t.Parallel()
	got := flattenSystem(Request{
		System: "BASE",
		SystemSections: []SystemSection{
			{Name: "base", Body: "you are a tester"},
			{Name: "memory", Body: "<memory-context>likes cats</memory-context>"},
			{Name: "auto-retrieve", Body: "<auto-retrieve>passage about cats</auto-retrieve>"},
		},
	})
	// All three bodies present in order.
	for i, want := range []string{"you are a tester", "<memory-context>", "<auto-retrieve>"} {
		if !strings.Contains(got, want) {
			t.Errorf("section %d body missing %q in:\n%s", i, want, got)
		}
	}
	// Sections separated by blank line so block markers stay parseable.
	if !strings.Contains(got, "\n\n") {
		t.Errorf("expected \\n\\n separator between sections; got:\n%s", got)
	}
	// Memory must come before auto-retrieve (insertion order preserved).
	if strings.Index(got, "memory-context") > strings.Index(got, "auto-retrieve") {
		t.Errorf("section order not preserved:\n%s", got)
	}
	// req.System ignored when sections present (sections ARE the
	// system prompt; mixing both would duplicate the base block).
	if strings.Contains(got, "BASE") {
		t.Errorf("req.System leaked when SystemSections populated; got:\n%s", got)
	}
}

func TestFlattenSystem_SkipsEmptyBodies(t *testing.T) {
	t.Parallel()
	got := flattenSystem(Request{
		SystemSections: []SystemSection{
			{Name: "base", Body: "BASE"},
			{Name: "empty", Body: ""},
			{Name: "tail", Body: "TAIL"},
		},
	})
	if !strings.Contains(got, "BASE") || !strings.Contains(got, "TAIL") {
		t.Errorf("BASE/TAIL missing: %q", got)
	}
	if strings.Count(got, "\n\n") > 1 {
		t.Errorf("empty body produced extra \\n\\n separator: %q", got)
	}
}

func TestToOpenAI_SystemSections_LandsInSystemMessage(t *testing.T) {
	t.Parallel()
	// End-to-end: feed Request with SystemSections through toOpenAI
	// and confirm the resulting oaiReq has the joined system message
	// in Messages[0].
	req := Request{
		Stream: true,
		SystemSections: []SystemSection{
			{Name: "base", Body: "you are metis"},
			{Name: "auto-retrieve", Body: "[1] cat is teal blue"},
		},
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}
	oai := toOpenAI(req, "test-model", 1024)
	if len(oai.Messages) < 2 {
		t.Fatalf("expected ≥2 messages (system + user); got %d", len(oai.Messages))
	}
	sys := oai.Messages[0]
	if sys.Role != "system" {
		t.Fatalf("first message role = %q, want system", sys.Role)
	}
	sysContent, _ := sys.Content.(string)
	if !strings.Contains(sysContent, "you are metis") {
		t.Errorf("system message missing base body: %q", sysContent)
	}
	if !strings.Contains(sysContent, "cat is teal blue") {
		t.Errorf("system message missing auto-retrieve body — section serialization broken: %q", sysContent)
	}
}

func TestToOpenAI_SystemSectionsTakePrecedence(t *testing.T) {
	t.Parallel()
	// When SystemSections are set, req.System is ignored — sections
	// already include everything. Pre-fix would have had no system
	// message at all (System="" and sections dropped); now we have
	// the section content.
	req := Request{
		Stream:         true,
		System:         "this should NOT appear (SystemSections wins)",
		SystemSections: []SystemSection{{Name: "base", Body: "this SHOULD appear"}},
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}
	oai := toOpenAI(req, "m", 1024)
	sys, _ := oai.Messages[0].Content.(string)
	if strings.Contains(sys, "should NOT appear") {
		t.Errorf("legacy req.System leaked: %q", sys)
	}
	if !strings.Contains(sys, "this SHOULD appear") {
		t.Errorf("section body missing: %q", sys)
	}
}
