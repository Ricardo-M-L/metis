package agent

// build_request_sections_test.go — locks the Loop.buildRequest path
// that turns Loop.SystemSections + Memory.BuildContext() into the
// outgoing llm.Request.SystemSections.
//
// The bug this prevents: pre-fix, memory was concatenated into the
// System string. That broke per-section caching because the Anthropic
// provider couldn't see "memory is dynamic, never cache" — every
// memory write invalidated the addendum cache, defeating the whole
// point of prompt caching for long-running sessions.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

func TestBuildRequest_AppendsMemoryAsVolatileSection(t *testing.T) {
	dir := t.TempDir()
	mm, err := memory.NewMemoryManager(dir)
	if err != nil {
		t.Fatalf("memory manager: %v", err)
	}
	// Seed at least one memory entry so BuildContext returns non-empty.
	if err := mm.Core().UpdateBlock("user", "I'm a senior Go dev who hates fluffy comments"); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	l := &Loop{
		System: "BASE-PROMPT",
		SystemSections: []llm.SystemSection{
			{Name: "base", Body: "BASE-PROMPT", Cache: true},
			{Name: "env", Body: "<env>...</env>", Cache: false, Volatile: true},
			{Name: "addendum", Body: "ADDENDUM", Cache: true},
		},
		Memory:   mm,
		Provider: nil,
	}
	req := l.buildRequest(nil)

	// Memory section must be appended at the END with Volatile=true.
	if len(req.SystemSections) != 4 {
		t.Fatalf("expected 4 sections (base + env + addendum + memory), got %d", len(req.SystemSections))
	}
	last := req.SystemSections[3]
	if last.Name != "memory" {
		t.Errorf("last section should be 'memory', got %q", last.Name)
	}
	if !last.Volatile {
		t.Error("memory section MUST be Volatile=true (defeats the cache bug)")
	}
	if last.Cache {
		t.Error("memory section must not be marked Cache=true (Volatile would override but the intent should be clear)")
	}
	if last.Body == "" {
		t.Error("memory section body is empty — BuildContext returned nothing")
	}
}

func TestBuildRequest_LegacyPathConcatsMemory(t *testing.T) {
	// When SystemSections is nil, the legacy path appends memory to
	// the System string. The cache benefit is lost, but at least the
	// content reaches the model.
	dir := t.TempDir()
	mm, _ := memory.NewMemoryManager(dir)
	_ = mm.Core().UpdateBlock("user", "legacy-memory-body")

	l := &Loop{
		System:         "STRING-ONLY-BASE",
		SystemSections: nil, // explicitly legacy path
		Memory:         mm,
	}
	req := l.buildRequest(nil)

	if len(req.SystemSections) != 0 {
		t.Errorf("legacy path should leave SystemSections empty, got %d sections", len(req.SystemSections))
	}
	if req.System == "STRING-ONLY-BASE" {
		t.Errorf("legacy path: memory should have been appended to System; got %q", req.System)
	}
	if !contains(req.System, "legacy-memory-body") {
		t.Errorf("legacy path: memory body missing from System: %q", req.System)
	}
}

func TestBuildRequest_NoMemoryNoSection(t *testing.T) {
	// Without a MemoryManager, no memory section should be emitted —
	// the prompt slice is exactly what the caller set on Loop.
	l := &Loop{
		System: "BASE",
		SystemSections: []llm.SystemSection{
			{Name: "base", Body: "BASE", Cache: true},
		},
	}
	req := l.buildRequest(nil)
	if len(req.SystemSections) != 1 {
		t.Errorf("expected exactly 1 section when no memory; got %d", len(req.SystemSections))
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
