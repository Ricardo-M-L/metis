package agent

// build_request_sections_test.go — locks the Loop.buildRequest path
// that turns Loop.SystemSections + Memory.BuildContext() into the
// outgoing llm.Request.SystemSections.
//
// The bug this prevents: pre-fix, memory was concatenated into the
// System string. That broke per-section caching because the provider could
// not reuse the stable short memory index independently from query-specific
// recall. The index is byte-stable until memory changes; recalled bodies live
// in a synthetic user tail during live turns.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

func TestBuildRequest_AppendsStableMemoryIndexSection(t *testing.T) {
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

	// The short index is stable and cacheable. Query-specific bodies are not
	// part of this section and are attached at the conversation tail by Run.
	if len(req.SystemSections) != 4 {
		t.Fatalf("expected 4 sections (base + env + addendum + memory), got %d", len(req.SystemSections))
	}
	last := req.SystemSections[3]
	if last.Name != "memory_index" {
		t.Errorf("last section should be 'memory_index', got %q", last.Name)
	}
	if last.Volatile {
		t.Error("stable memory index must not be volatile")
	}
	if !last.Cache {
		t.Error("stable memory index must be cacheable")
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

func TestAppendUserClearsPreviousTurnMemorySnapshot(t *testing.T) {
	l := &Loop{
		turnMemoryContext:        "old-index",
		turnMemoryRecall:         "old-recall",
		turnMemoryPrepared:       true,
		turnMemoryRecallAttached: true,
	}
	l.AppendUser("new query")
	if l.turnMemoryPrepared || l.turnMemoryRecallAttached || l.turnMemoryContext != "" || l.turnMemoryRecall != "" {
		t.Fatalf("new user turn retained old memory snapshot: context=%q recall=%q prepared=%v attached=%v",
			l.turnMemoryContext, l.turnMemoryRecall, l.turnMemoryPrepared, l.turnMemoryRecallAttached)
	}
}

func TestBuildRequest_ReinjectsCurrentStateAfterHistoryReplacement(t *testing.T) {
	state := "permission_mode: ask"
	l := &Loop{
		System: "BASE",
		SystemSections: []llm.SystemSection{
			{Name: "base", Body: "BASE", Cache: true},
		},
		CurrentStateSections: func() []llm.SystemSection {
			return []llm.SystemSection{{Name: "runtime_state", Body: state, Cache: true}}
		},
	}
	first := l.buildRequest(nil)
	state = "permission_mode: bypassPermissions\n<current_plan>ship release</current_plan>"
	// Simulate a compaction replacement between requests. Dynamic state must
	// come from the callback, not from the old message prefix.
	l.Messages = []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "<checkpoint/>"}}}}
	second := l.buildRequest(nil)
	if len(first.SystemSections) != 2 || len(second.SystemSections) != 2 {
		t.Fatalf("dynamic section counts: first=%d second=%d", len(first.SystemSections), len(second.SystemSections))
	}
	if contains(first.SystemSections[1].Body, "bypassPermissions") {
		t.Fatal("first request unexpectedly saw future state")
	}
	got := second.SystemSections[1]
	if !contains(got.Body, "bypassPermissions") || !contains(got.Body, "ship release") {
		t.Fatalf("second request missed fresh state: %+v", got)
	}
	if got.Cache || !got.Volatile {
		t.Fatalf("current state must be volatile/non-cacheable: %+v", got)
	}
}

func TestBuildRequest_ReusesStructuredRuntimeSnapshotUntilStateChanges(t *testing.T) {
	state := RuntimeStateSnapshot{
		PermissionMode:   "default",
		WorkingDirectory: "/work/project",
		SessionID:        "session-a",
		CurrentPlan:      "ship the cache fix",
	}
	calls := 0
	l := &Loop{
		System: "BASE",
		SystemSections: []llm.SystemSection{
			{Name: "base", Body: "BASE", Cache: true},
		},
		Model: "model-a",
		CurrentStateSnapshot: func() RuntimeStateSnapshot {
			calls++
			return state
		},
	}

	first := l.buildRequest(nil)
	second := l.buildRequest(nil)
	firstState := sectionNamed(t, first.SystemSections, "runtime_state")
	secondState := sectionNamed(t, second.SystemSections, "runtime_state")
	if calls != 2 {
		t.Fatalf("authoritative state reads = %d, want 2", calls)
	}
	if l.runtimeStateRevision != 1 {
		t.Fatalf("unchanged state rendered %d times, want 1", l.runtimeStateRevision)
	}
	if firstState.Body != secondState.Body {
		t.Fatal("unchanged state did not reuse byte-identical body")
	}
	if !firstState.Cache || firstState.Volatile {
		t.Fatalf("structured runtime state must be stable/cacheable: %+v", firstState)
	}
	if !contains(firstState.Body, "model: model-a") || !contains(firstState.Body, "plan_mode: false") {
		t.Fatalf("runtime-owned fields missing: %q", firstState.Body)
	}

	state.PermissionMode = "bypassPermissions"
	third := l.buildRequest(nil)
	thirdState := sectionNamed(t, third.SystemSections, "runtime_state")
	if l.runtimeStateRevision != 2 {
		t.Fatalf("changed state revision = %d, want 2", l.runtimeStateRevision)
	}
	if thirdState.Body == secondState.Body || !contains(thirdState.Body, "bypassPermissions") {
		t.Fatalf("changed state was not rendered: %q", thirdState.Body)
	}
}

func TestBuildRequest_RuntimeSnapshotRefreshesAfterRestoreAndPlanChange(t *testing.T) {
	l := &Loop{
		System: "BASE",
		SystemSections: []llm.SystemSection{
			{Name: "base", Body: "BASE", Cache: true},
		},
		CurrentStateSnapshot: func() RuntimeStateSnapshot {
			return RuntimeStateSnapshot{PermissionMode: "default"}
		},
	}
	_ = l.buildRequest(nil)
	if l.runtimeStateRevision != 1 {
		t.Fatalf("initial revision = %d, want 1", l.runtimeStateRevision)
	}

	l.Restore([]llm.Message{{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Type: "text", Text: "restored"}},
	}})
	restored := l.buildRequest(nil)
	if l.runtimeStateRevision != 2 {
		t.Fatalf("restore did not force a full snapshot: revision=%d", l.runtimeStateRevision)
	}

	l.SetPlanMode(true)
	planned := l.buildRequest(nil)
	if l.runtimeStateRevision != 3 {
		t.Fatalf("plan change revision = %d, want 3", l.runtimeStateRevision)
	}
	if !contains(sectionNamed(t, planned.SystemSections, "runtime_state").Body, "plan_mode: true") {
		t.Fatal("plan mode was not projected into runtime state")
	}
	if sectionNamed(t, restored.SystemSections, "runtime_state").Body == sectionNamed(t, planned.SystemSections, "runtime_state").Body {
		t.Fatal("plan transition reused stale runtime-state bytes")
	}
}

func sectionNamed(t *testing.T, sections []llm.SystemSection, name string) llm.SystemSection {
	t.Helper()
	for _, section := range sections {
		if section.Name == name {
			return section
		}
	}
	t.Fatalf("section %q not found in %+v", name, sections)
	return llm.SystemSection{}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
