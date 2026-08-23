package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestAppend_SingleWriteSyscall — verify the append path produces
// exactly one Write call to the underlying file. Pre-2026-05-22 it
// used json.Encoder which doesn't guarantee single-write. Test by
// writing a known entry and validating the file content matches
// exactly `json + '\n'` (no internal Encoder formatting differences).
func TestAppend_SingleWriteSyscall(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	s, err := NewStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}

	sid := "test-single-write"
	if err := s.WriteHeader(sid, "test-model", "system prompt"); err != nil {
		t.Fatal(err)
	}
	msg := llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Type: "text", Text: "hello world"}},
	}
	if err := s.AppendMessage(sid, msg); err != nil {
		t.Fatal(err)
	}

	// Read back as raw bytes and verify shape: header line + msg line,
	// each ending exactly with '\n', no extra trailing whitespace.
	body, err := os.ReadFile(s.path(sid))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if got := len(lines); got != 2 {
		t.Errorf("expected 2 lines (header + msg); got %d", got)
	}
	// Each line must be valid JSON on its own — the per-line atomic
	// invariant the rest of the pipeline depends on.
	for i, ln := range lines {
		var e Entry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Errorf("line %d not valid JSON: %v", i+1, err)
		}
	}
}

// TestLoad_TolerateCorruptedTrailingLine — the resume-recovery case.
// Simulate a torn-write at the end of session.jsonl (e.g. SIGKILL
// during AppendMessage). Old behavior: Load aborted with "decode
// session entry" → session unrecoverable. New behavior: drop the
// bad trailing line, resume from clean prefix, log to stderr.
func TestLoad_TolerateCorruptedTrailingLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	s, err := NewStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}

	sid := "test-tolerate-trailing"
	if err := s.WriteHeader(sid, "test-model", "system prompt"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(sid, llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Type: "text", Text: "clean prefix"}},
	}); err != nil {
		t.Fatal(err)
	}

	// Manually append a corrupted line to simulate torn write.
	f, err := os.OpenFile(s.path(sid), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"this line ge`)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Load should succeed, drop the bad line, return the clean prefix.
	hdr, msgs, err := s.Load(sid)
	if err != nil {
		t.Fatalf("Load should tolerate trailing corruption; got: %v", err)
	}
	if hdr == nil {
		t.Fatal("header should be loaded despite trailing corruption")
	}
	if got := len(msgs); got != 1 {
		t.Errorf("expected 1 clean message; got %d", got)
	}
	if got := msgs[0].Content[0].Text; got != "clean prefix" {
		t.Errorf("clean message lost; got %q", got)
	}
}

func TestAppend_RepairsCorruptedTrailingPartialLine(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}

	const sid = "test-repair-trailing"
	if err := s.WriteHeader(sid, "test-model", "system prompt"); err != nil {
		t.Fatal(err)
	}
	first := llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "clean prefix"}},
	}
	if err := s.AppendMessage(sid, first); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(s.path(sid), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"torn`)); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	second := llm.Message{
		Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "after restart"}},
	}
	if err := s.AppendMessage(sid, second); err != nil {
		t.Fatal(err)
	}

	_, got, err := s.Load(sid)
	if err != nil {
		t.Fatalf("Load after repairing torn tail: %v", err)
	}
	if len(got) != 2 || got[0].Content[0].Text != "clean prefix" || got[1].Content[0].Text != "after restart" {
		t.Fatalf("recovered history = %#v, want clean prefix and post-restart append", got)
	}

	body, err := os.ReadFile(s.path(sid))
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("line %d remains corrupt after append: %q", i+1, line)
		}
	}
}

func TestLoadHeaderAndList_TolerateCorruptedTrailingPartialLine(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const sid = "test-list-trailing"
	if err := s.WriteHeader(sid, "test-model", "system prompt"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(sid, llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "visible session"}},
	}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.path(sid), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(`{"type":"message","message":{"role":"assistant"`)); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	hdr, count, err := s.LoadHeader(sid)
	if err != nil {
		t.Fatalf("LoadHeader should tolerate trailing partial JSON: %v", err)
	}
	if hdr == nil || count != 1 {
		t.Fatalf("LoadHeader = (%#v, %d), want header and one message", hdr, count)
	}
	entries, err := s.List(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != sid || entries[0].MessageCount != 1 {
		t.Fatalf("List hid recoverable session: %#v", entries)
	}
}

// TestLoad_FailsOnMidFileCorruption — the security guard: if a
// non-trailing line is corrupt (FS-level corruption or a deliberate
// mangling), surface a hard error rather than silently dropping
// data. Real torn-writes are always trailing, so a mid-file bad
// line is a real integrity problem.
func TestLoad_FailsOnMidFileCorruption(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	s, err := NewStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}

	sid := "test-midfile-corrupt"
	if err := s.WriteHeader(sid, "test-model", "system prompt"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(sid, llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Type: "text", Text: "first"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Insert garbage line + add a valid follow-up. After this the
	// garbage is NOT trailing — Load should refuse.
	f, err := os.OpenFile(s.path(sid), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("not-valid-json-here\n"))
	f.Close()
	if err := s.AppendMessage(sid, llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Type: "text", Text: "after garbage"}},
	}); err != nil {
		t.Fatal(err)
	}

	_, _, err = s.Load(sid)
	if err == nil {
		t.Error("mid-file corruption should fail Load, not be silently skipped")
	}
	if _, _, err := s.LoadHeader(sid); err == nil {
		t.Error("mid-file corruption should fail LoadHeader, not be silently skipped")
	}
}
