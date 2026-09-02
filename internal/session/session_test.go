package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestSession_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := store.NewSessionID()
	if err := store.WriteHeader(id, "test-model", "you are a test"); err != nil {
		t.Fatal(err)
	}
	want := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hi"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}},
	}
	for _, m := range want {
		if err := store.AppendMessage(id, m); err != nil {
			t.Fatal(err)
		}
	}
	hdr, got, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Model != "test-model" {
		t.Errorf("model=%q", hdr.Model)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Role != w.Role {
			t.Errorf("msg %d role=%q want %q", i, got[i].Role, w.Role)
		}
		if got[i].Content[0].Text != w.Content[0].Text {
			t.Errorf("msg %d text=%q want %q", i, got[i].Content[0].Text, w.Content[0].Text)
		}
	}
}

func TestSession_LoadCanonicalizesLegacyNilToolInput(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := store.NewSessionID()
	if err := store.WriteHeader(id, "test-model", ""); err != nil {
		t.Fatal(err)
	}
	// ToolInput is nil, matching old name-only calls. Because the field is
	// omitempty, this also exercises the exact on-disk shape of the affected
	// session rather than manufacturing a special fixture.
	if err := store.AppendMessage(id, llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUseID: "call_truncated", ToolName: "Write",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	_, messages, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || len(messages[0].Content) != 1 {
		t.Fatalf("messages = %+v", messages)
	}
	input := messages[0].Content[0].ToolInput
	if input == nil {
		t.Fatal("loaded legacy tool input is nil; want empty object")
	}
	if len(input) != 0 {
		t.Fatalf("loaded tool input = %+v, want empty object", input)
	}
}

func TestSession_List(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	for _, id := range []string{"a", "b", "c"} {
		_ = store.WriteHeader(id, "m", "")
	}
	es, err := store.List(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 3 {
		t.Errorf("got %d sessions, want 3", len(es))
	}
}

func TestListResumableFiltersEmptySubagentAndOtherWorkspace(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(t.TempDir(), "current")
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(current, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	store, _ := NewStore(dir)
	write := func(h Header, messages ...llm.Message) {
		t.Helper()
		if err := store.WriteHeaderFull(h); err != nil {
			t.Fatal(err)
		}
		for _, message := range messages {
			if err := store.AppendMessage(h.ID, message); err != nil {
				t.Fatal(err)
			}
		}
	}
	user := func(text string) llm.Message {
		return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: text}}}
	}
	write(Header{ID: "empty", WorkDir: current, Model: "m"})
	write(Header{ID: "other", WorkDir: other, Model: "m"}, user("other prompt"))
	write(Header{ID: "child", WorkDir: current, Model: "m", SubAgentOf: "parent"}, user("child prompt"))
	write(Header{ID: "current", WorkDir: current, Model: "m"}, user("fix the resume picker"))

	got, err := store.ListResumable(ResumeListOptions{Limit: 20, WorkDir: current})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "current" {
		t.Fatalf("resumable = %#v, want only current", got)
	}
	if got[0].MessageCount != 1 || got[0].Title != "fix the resume picker" {
		t.Fatalf("metadata = %#v, want message count and first-prompt title", got[0])
	}
}

func TestListResumableIncludesSameRepositorySubdirectory(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	subdir := filepath.Join(repo, "nested", "package")
	otherRepo := filepath.Join(t.TempDir(), "other-repo")
	for _, dir := range []string{filepath.Join(repo, ".git"), subdir, filepath.Join(otherRepo, ".git")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	store, _ := NewStore(t.TempDir())
	user := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "same repository"}}}
	for _, header := range []Header{
		{ID: "repo-root", WorkDir: repo, Model: "m"},
		{ID: "other-repo", WorkDir: otherRepo, Model: "m"},
	} {
		if err := store.WriteHeaderFull(header); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessage(header.ID, user); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ListResumable(ResumeListOptions{Limit: 20, WorkDir: subdir})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "repo-root" {
		t.Fatalf("resumable from repo subdirectory = %#v, want repo-root", got)
	}
}

func TestListUsesLatestActivityAndHistoryReplaceCount(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	base := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	for _, id := range []string{"older-created", "newer-created"} {
		if err := store.WriteHeaderFull(Header{ID: id, CreatedAt: base, Model: "m"}); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessage(id, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: id}}}); err != nil {
			t.Fatal(err)
		}
	}
	olderPath := store.path("older-created")
	newerPath := store.path("newer-created")
	if err := os.Chtimes(newerPath, base.Add(time.Minute), base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(olderPath, base.Add(2*time.Minute), base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceHistoryAndMark("older-created", []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "replacement"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "answer"}}},
	}, &HistoryCursor{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(olderPath, base.Add(3*time.Minute), base.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	got, err := store.List(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "older-created" || got[0].MessageCount != 2 || got[0].Title != "replacement" {
		t.Fatalf("list = %#v", got)
	}
}

func TestSession_ExportImportRoundTrip(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	src, _ := NewStore(dirA)
	id := src.NewSessionID()
	_ = src.WriteHeader(id, "model-x", "you are helpful")
	_ = src.AppendMessage(id, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "ping"}}})
	_ = src.AppendMessage(id, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "pong"}}})

	var buf bytes.Buffer
	if err := src.Export(id, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if !strings.Contains(buf.String(), "ping") || !strings.Contains(buf.String(), "pong") {
		t.Fatalf("export missing content: %q", buf.String())
	}

	dst, _ := NewStore(dirB)
	newID, err := dst.Import(&buf, "")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if newID == "" {
		t.Fatal("Import returned empty id")
	}

	hdr, msgs, err := dst.Load(newID)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Model != "model-x" {
		t.Errorf("model=%q after import", hdr.Model)
	}
	if hdr.ID != newID {
		t.Errorf("imported header id should be rewritten, got %q want %q", hdr.ID, newID)
	}
	if len(msgs) != 2 || msgs[0].Content[0].Text != "ping" || msgs[1].Content[0].Text != "pong" {
		t.Errorf("messages mismatch after import: %+v", msgs)
	}
}

func TestSession_ImportPreferredID(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	src, _ := NewStore(t.TempDir())
	id := src.NewSessionID()
	src.WriteHeader(id, "m", "")
	src.AppendMessage(id, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "x"}}})

	var buf bytes.Buffer
	src.Export(id, &buf)

	got, err := store.Import(&buf, "my-fixed-id")
	if err != nil {
		t.Fatal(err)
	}
	if got != "my-fixed-id" {
		t.Errorf("got id %q, want my-fixed-id", got)
	}
}

func TestSession_ImportStripsPersistedPermissionState(t *testing.T) {
	src, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := src.WriteHeaderFull(Header{
		ID:               "external",
		CreatedAt:        time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
		Provider:         "openai",
		Model:            "gpt-test",
		System:           "keep this custom system prompt",
		SystemPromptKind: SystemPromptKindCustom,
		WorkDir:          "/tmp/imported-project",
		Mode:             "plan",
		PrePlanMode:      "fullAccess",
		Effort:           "high",
		Preset:           "custom-preset",
		AlwaysAllow: []SavedRule{{
			Tool:   "Bash",
			Match:  "*",
			Verb:   1,
			Source: "interactive",
		}},
		ClearAlwaysAllow: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := src.AppendMessage("external", llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Type: "text", Text: "preserve me"}},
	}); err != nil {
		t.Fatal(err)
	}

	var exported bytes.Buffer
	if err := src.Export("external", &exported); err != nil {
		t.Fatal(err)
	}
	dst, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	importedID, err := dst.Import(&exported, "sanitized")
	if err != nil {
		t.Fatal(err)
	}
	hdr, msgs, err := dst.Load(importedID)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Mode != "" || hdr.PrePlanMode != "" || len(hdr.AlwaysAllow) != 0 || hdr.ClearAlwaysAllow {
		t.Fatalf("import retained permission state: mode=%q pre_plan_mode=%q always_allow=%+v clear_always_allow=%v",
			hdr.Mode, hdr.PrePlanMode, hdr.AlwaysAllow, hdr.ClearAlwaysAllow)
	}
	if hdr.Provider != "openai" || hdr.Model != "gpt-test" ||
		hdr.System != "keep this custom system prompt" || hdr.SystemPromptKind != SystemPromptKindCustom ||
		hdr.WorkDir != "/tmp/imported-project" || hdr.Effort != "high" || hdr.Preset != "custom-preset" {
		t.Fatalf("import lost ordinary session metadata: %+v", hdr)
	}
	if len(msgs) != 1 || msgs[0].Content[0].Text != "preserve me" {
		t.Fatalf("import lost conversation content: %+v", msgs)
	}
}

func TestSession_ImportStripsDirectFullAccessMode(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	input := `{"type":"header","header":{"id":"external","created_at":"2026-09-02T12:00:00Z","model":"gpt-test","mode":"fullAccess","pre_plan_mode":"bypassPermissions","always_allow":[{"tool":"Bash","match":"*","verb":1}],"clear_always_allow":true}}` + "\n"
	importedID, err := store.Import(strings.NewReader(input), "direct-full-access")
	if err != nil {
		t.Fatal(err)
	}
	hdr, _, err := store.LoadHeader(importedID)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Mode != "" || hdr.PrePlanMode != "" || len(hdr.AlwaysAllow) != 0 || hdr.ClearAlwaysAllow {
		t.Fatalf("import retained direct authority: %+v", hdr)
	}
}

func TestSession_ImportRejectsExistingID(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	store.WriteHeader("dup", "m", "")

	src, _ := NewStore(t.TempDir())
	src.WriteHeader("anything", "m", "")
	var buf bytes.Buffer
	src.Export("anything", &buf)

	if _, err := store.Import(&buf, "dup"); err == nil {
		t.Error("expected error when importing into existing id")
	}
}

func TestSession_ImportRejectsBadInput(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	if _, err := store.Import(strings.NewReader(""), ""); err == nil {
		t.Error("expected error on empty input")
	}
	if _, err := store.Import(strings.NewReader("not-json\n"), ""); err == nil {
		t.Error("expected error on garbage input")
	}
	// Header is fine but second line is wrong type
	bad := `{"type":"header","header":{"id":"x","created_at":"2026-04-28T00:00:00Z","model":"m"}}` + "\n" +
		`{"type":"unknown"}` + "\n"
	if _, err := store.Import(strings.NewReader(bad), ""); err == nil {
		t.Error("expected error when non-message entry follows header")
	}
}

func TestSession_WriteHeaderFullPreservesAllFields(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	rules := []SavedRule{
		{Tool: "Bash", Match: "git", Verb: 1, Source: "user"},
		{Tool: "Read", Verb: 1, Source: "config"},
	}
	want := Header{
		ID:          "resume-test",
		Provider:    "anthropic",
		Model:       "claude-opus",
		System:      "you are helpful",
		WorkDir:     "/tmp/scratch",
		Mode:        "auto",
		Effort:      "high",
		Preset:      "creator",
		Status:      "completed",
		AlwaysAllow: rules,
	}
	if err := store.WriteHeaderFull(want); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.Load("resume-test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "anthropic" || got.Mode != "auto" || got.WorkDir != "/tmp/scratch" || got.Effort != "high" || got.Preset != "creator" || got.Status != "completed" {
		t.Errorf("header roundtrip wrong: %+v", got)
	}
	if len(got.AlwaysAllow) != 2 {
		t.Fatalf("expected 2 always-allow rules, got %d", len(got.AlwaysAllow))
	}
	if got.AlwaysAllow[0].Tool != "Bash" || got.AlwaysAllow[0].Match != "git" {
		t.Errorf("rule[0] mismatch: %+v", got.AlwaysAllow[0])
	}
}

func TestSession_HeaderProviderMergesAndBranchPreservesIt(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const parentID = "provider-parent"
	if err := store.WriteHeaderFull(Header{
		ID: parentID, Provider: "anthropic", Model: "claude-old", System: "parent-system",
	}); err != nil {
		t.Fatal(err)
	}
	// A title-only append must not erase the provider. A later explicit
	// provider/model append must replace both fields together.
	if err := store.SetTitle(parentID, "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeaderFull(Header{ID: parentID, Provider: "openai", Model: "gpt-new"}); err != nil {
		t.Fatal(err)
	}
	hdr, _, err := store.LoadHeader(parentID)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Provider != "openai" || hdr.Model != "gpt-new" || hdr.System != "parent-system" || hdr.Title != "renamed" {
		t.Fatalf("merged header = %+v", hdr)
	}

	branchID, err := store.Branch(parentID, nil)
	if err != nil {
		t.Fatal(err)
	}
	branch, _, err := store.LoadHeader(branchID)
	if err != nil {
		t.Fatal(err)
	}
	if branch.Provider != "openai" || branch.Model != "gpt-new" || branch.System != "parent-system" || branch.Status != "idle" {
		t.Fatalf("branch lost provider/model/system: %+v", branch)
	}
}

func TestSession_ClearAlwaysAllowTombstone(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "clear-rules"
	if err := store.WriteHeaderFull(Header{
		ID: id, Model: "test",
		AlwaysAllow: []SavedRule{{Tool: "Edit", Verb: 1, Source: "interactive"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeaderFull(Header{ID: id, ClearAlwaysAllow: true}); err != nil {
		t.Fatal(err)
	}
	for _, load := range []struct {
		name string
		fn   func() (*Header, error)
	}{
		{name: "Load", fn: func() (*Header, error) { h, _, err := store.Load(id); return h, err }},
		{name: "LoadHeader", fn: func() (*Header, error) { h, _, err := store.LoadHeader(id); return h, err }},
	} {
		t.Run(load.name, func(t *testing.T) {
			hdr, err := load.fn()
			if err != nil {
				t.Fatal(err)
			}
			if len(hdr.AlwaysAllow) != 0 {
				t.Fatalf("tombstone did not clear rules: %+v", hdr.AlwaysAllow)
			}
		})
	}

	// The tombstone clears prior state; it must not permanently prevent a
	// later interactive approval from being persisted.
	if err := store.WriteHeaderFull(Header{
		ID: id, AlwaysAllow: []SavedRule{{Tool: "Read", Verb: 1, Source: "interactive"}},
	}); err != nil {
		t.Fatal(err)
	}
	hdr, _, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(hdr.AlwaysAllow) != 1 || hdr.AlwaysAllow[0].Tool != "Read" {
		t.Fatalf("rule written after tombstone was not restored: %+v", hdr.AlwaysAllow)
	}
}

func TestSession_LoadHeaderOnly(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	id := "abc"
	_ = store.WriteHeader(id, "model-x", "sys")
	hdr, _, err := store.LoadHeader(id)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.ID != id || hdr.Model != "model-x" {
		t.Errorf("hdr=%+v", hdr)
	}
	// Verify file exists
	if _, err := os.Stat(filepath.Join(dir, id+".jsonl")); err != nil {
		t.Errorf("session file missing: %v", err)
	}
}
