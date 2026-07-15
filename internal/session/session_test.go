package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		AlwaysAllow: rules,
	}
	if err := store.WriteHeaderFull(want); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.Load("resume-test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "anthropic" || got.Mode != "auto" || got.WorkDir != "/tmp/scratch" {
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
	if branch.Provider != "openai" || branch.Model != "gpt-new" || branch.System != "parent-system" {
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
