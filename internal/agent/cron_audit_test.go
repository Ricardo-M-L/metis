package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenAuditLog_CreatesDirAndFile(t *testing.T) {
	root := t.TempDir()
	w, err := OpenAuditLog(root, "job-abc")
	if err != nil {
		t.Fatalf("OpenAuditLog: %v", err)
	}
	defer func() { _ = w.Close() }()

	if w.Path() == "" {
		t.Fatal("Path() returned empty")
	}
	auditDir := filepath.Join(root, "audit", "job-abc")
	st, err := os.Stat(auditDir)
	if err != nil || !st.IsDir() {
		t.Fatalf("audit dir not created: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(w.Path()), time.Now().UTC().Format("2006-01-02")) {
		t.Errorf("audit filename should be date-prefixed; got %s", filepath.Base(w.Path()))
	}
}

func TestAuditWriter_AppendThenReadJSONL(t *testing.T) {
	root := t.TempDir()
	w, err := OpenAuditLog(root, "j1")
	if err != nil {
		t.Fatal(err)
	}
	w.Append(AuditEntry{Kind: "start", Text: "job 1"})
	w.Append(AuditEntry{Kind: "tool_start", Tool: "Bash"})
	w.Append(AuditEntry{Kind: "tool_result", Text: "ok", IsError: false})
	w.Append(AuditEntry{Kind: "loop_done"})
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	data, err := os.ReadFile(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 jsonl entries; got %d:\n%s", len(lines), data)
	}
	for i, ln := range lines {
		var e AuditEntry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Errorf("line %d: bad JSON: %v", i, err)
		}
		if e.At.IsZero() {
			t.Errorf("line %d: At not stamped", i)
		}
	}
}

func TestAuditWriter_NilSafeAppendClose(t *testing.T) {
	// Defensive — callers reach for AuditWriter when job.Silent is true,
	// but OpenAuditLog can fail (disk full, EROFS). The downstream code
	// shouldn't panic if it forgot to nil-check.
	var w *AuditWriter
	w.Append(AuditEntry{Kind: "start"}) // must not panic
	if err := w.Close(); err != nil {
		t.Errorf("nil Close should return nil; got %v", err)
	}
}

func TestCronServiceAuditPath_MissingDirReturnsFalse(t *testing.T) {
	root := t.TempDir()
	svc, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	_, ok := svc.AuditPath("never-fired")
	if ok {
		t.Error("AuditPath should return false for a job with no fires")
	}
}

func TestCronServiceListAuditFires_NewestFirst(t *testing.T) {
	root := t.TempDir()
	svc, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	// Manually plant three files with sortable-by-name timestamps.
	dir := filepath.Join(root, "audit", "job-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ts := range []string{
		"2026-05-13T01-00-00Z.jsonl",
		"2026-05-13T03-00-00Z.jsonl",
		"2026-05-13T02-00-00Z.jsonl",
	} {
		if err := os.WriteFile(filepath.Join(dir, ts), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	names, err := svc.ListAuditFires("job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Fatalf("expected 3 fires; got %v", names)
	}
	// Newest first (descending by filename, which is RFC3339-with-dashes).
	if names[0] != "2026-05-13T03-00-00Z.jsonl" {
		t.Errorf("newest first wrong; got %v", names)
	}
	if names[2] != "2026-05-13T01-00-00Z.jsonl" {
		t.Errorf("oldest last wrong; got %v", names)
	}
}

func TestCronJob_SilentDefaultsOff(t *testing.T) {
	// Existing jobs on disk (without the new Silent field) must
	// deserialize with Silent=false — JSON's zero-value rule handles
	// this but we pin it so a future refactor doesn't accidentally
	// change defaults (e.g., making all jobs silent breaks the user's
	// expectation that a fresh cron job streams to chat).
	raw := `{"id":"j","name":"n","prompt":"p","schedule":{"kind":"every","every_ms":60000},"enabled":true,"run_count":0,"created_at":"2026-05-13T00:00:00Z"}`
	var job CronJob
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		t.Fatal(err)
	}
	if job.Silent {
		t.Error("Silent must default to false for backwards compat with pre-2026-05-13 jobs")
	}
}
