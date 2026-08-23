package session

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestCheckResumeSize_BelowCapPasses(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Drop a small fake session file.
	id := "small-session"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckResumeSize(id); err != nil {
		t.Errorf("small session should pass, got: %v", err)
	}
}

func TestCheckResumeSize_OverCapErrors(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	// Force the physical-ledger override to a tiny cap so we don't have to
	// write the production-sized bounded ledger.
	t.Setenv("METIS_RESUME_PHYSICAL_MAX_MB", "1") // 1 MiB
	id := "big-session"
	// Write 2 MiB of zeros.
	big := make([]byte, 2*1024*1024)
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	err := store.CheckResumeSize(id)
	if err == nil {
		t.Fatal("over-cap session should error")
	}
	var rt *ResumeTooLargeError
	if !errors.As(err, &rt) {
		t.Fatalf("expected *ResumeTooLargeError, got %T: %v", err, err)
	}
	if rt.Bytes != int64(len(big)) {
		t.Errorf("Bytes: got %d, want %d", rt.Bytes, len(big))
	}
	if rt.CapBytes != 1024*1024 {
		t.Errorf("CapBytes: got %d, want %d", rt.CapBytes, 1024*1024)
	}
	if rt.Scope != ResumeSizePhysicalLedger {
		t.Errorf("Scope: got %q, want physical ledger", rt.Scope)
	}
}

func TestCheckResumeSize_MissingFileNotError(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	// No file written — Load will be the one to surface the missing-
	// file error; CheckResumeSize stays out of its way.
	if err := store.CheckResumeSize("does-not-exist"); err != nil {
		t.Errorf("missing file should return nil, got: %v", err)
	}
}

func TestCheckResumeSize_DisabledByZeroEnv(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	// User opts out — even huge files pass.
	t.Setenv("METIS_RESUME_PHYSICAL_MAX_MB", "0")
	id := "huge"
	big := make([]byte, 16*1024*1024)
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckResumeSize(id); err != nil {
		t.Errorf("disabled cap should accept any size, got: %v", err)
	}
}

func TestCheckResumeHistorySize_UsesLogicalReplacement(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	t.Setenv("METIS_RESUME_MAX_MB", "1")
	t.Setenv("METIS_RESUME_PHYSICAL_MAX_MB", "4")
	const id = "compacted-large-ledger"
	if err := store.WriteHeader(id, "model", "system"); err != nil {
		t.Fatal(err)
	}
	largeRaw := historyText(llm.RoleUser, strings.Repeat("x", 2*1024*1024))
	if err := store.AppendMessage(id, largeRaw); err != nil {
		t.Fatal(err)
	}
	cursor := NewHistoryCursor([]llm.Message{largeRaw})
	want := []llm.Message{historyText(llm.RoleAssistant, "small compacted checkpoint")}
	if err := store.ReplaceHistoryAndMark(id, want, &cursor); err != nil {
		t.Fatal(err)
	}

	if err := store.CheckResumeSize(id); err != nil {
		t.Fatalf("physical ledger below its independent cap should pass: %v", err)
	}
	_, got, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("logical history = %#v, want %#v", got, want)
	}
	if err := CheckResumeHistorySize(id, got); err != nil {
		t.Fatalf("small logical replacement should pass despite old raw ledger: %v", err)
	}
}

func TestCheckResumeHistorySize_RejectsLargeLogicalHistory(t *testing.T) {
	t.Setenv("METIS_RESUME_MAX_MB", "1")
	large := []llm.Message{historyText(llm.RoleUser, strings.Repeat("x", 2*1024*1024))}
	err := CheckResumeHistorySize("large-logical", large)
	if err == nil {
		t.Fatal("large logical history should be rejected")
	}
	var tooLarge *ResumeTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("error = %T %v, want ResumeTooLargeError", err, err)
	}
	if tooLarge.Scope != ResumeSizeLogicalHistory {
		t.Fatalf("scope = %q, want logical history", tooLarge.Scope)
	}
}

func TestResumeTooLargeError_MessageMentionsClearAndBranch(t *testing.T) {
	err := &ResumeTooLargeError{
		Path:     "/tmp/sessions/abc-123.jsonl",
		Bytes:    10 * 1024 * 1024,
		CapBytes: 8 * 1024 * 1024,
	}
	msg := err.Error()
	if !strings.Contains(msg, "10.0 MiB") {
		t.Errorf("size missing or wrong-format: %q", msg)
	}
	if !strings.Contains(msg, "8.0 MiB") {
		t.Errorf("cap missing: %q", msg)
	}
	if !strings.Contains(msg, "/clear") {
		t.Errorf("should hint /clear: %q", msg)
	}
	if !strings.Contains(msg, "/branch") {
		t.Errorf("should hint /branch: %q", msg)
	}
	if !strings.Contains(msg, "abc-123") {
		t.Errorf("should include short id for force-load hint: %q", msg)
	}
}

func TestFmtBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1024, "1.0 KiB"},
		{1500, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{8 * 1024 * 1024, "8.0 MiB"},
		{1363148, "1.3 MiB"}, // ~1.3 * 1024 * 1024
		{2 * 1024 * 1024 * 1024, "2.0 GiB"},
	}
	for _, c := range cases {
		if got := fmtBytes(c.n); got != c.want {
			t.Errorf("fmtBytes(%d)=%q, want %q", c.n, got, c.want)
		}
	}
}

func TestShortID_StripsPathAndExtension(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/a/b/c.jsonl", "c"},
		{"d.jsonl", "d"},
		{"no-ext", "no-ext"},
		{"/path/with/uuid-like-id.jsonl", "uuid-like-id"},
	}
	for _, c := range cases {
		if got := shortID(c.in); got != c.want {
			t.Errorf("shortID(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestResumeMaxBytes_DefaultIsEightMiBLogicalLimit(t *testing.T) {
	t.Setenv("METIS_RESUME_MAX_MB", "")
	got := resumeMaxBytes()
	if got != DefaultResumeMaxBytes {
		t.Errorf("default cap: got %d, want %d", got, DefaultResumeMaxBytes)
	}
}

func TestResumePhysicalMaxBytes_DefaultIsBoundedAboveLogicalLimit(t *testing.T) {
	t.Setenv("METIS_RESUME_PHYSICAL_MAX_MB", "")
	got := resumePhysicalMaxBytes()
	if got != DefaultResumePhysicalMaxBytes {
		t.Fatalf("default physical cap: got %d, want %d", got, DefaultResumePhysicalMaxBytes)
	}
	if got <= DefaultResumeMaxBytes {
		t.Fatalf("physical cap %d must exceed logical cap %d", got, DefaultResumeMaxBytes)
	}
}

func TestResumeScannerMaxBytesTracksPhysicalLimitAndStaysBoundedWhenDisabled(t *testing.T) {
	t.Setenv("METIS_RESUME_PHYSICAL_MAX_MB", "32")
	if got := resumeScannerMaxBytes(); got != 32*1024*1024 {
		t.Fatalf("scanner max = %d, want 32 MiB", got)
	}
	t.Setenv("METIS_RESUME_PHYSICAL_MAX_MB", "0")
	if got := resumeScannerMaxBytes(); got != int(DefaultResumePhysicalMaxBytes) {
		t.Fatalf("disabled physical guard scanner max = %d, want bounded default %d", got, DefaultResumePhysicalMaxBytes)
	}
}

func TestResumeMaxBytes_EnvOverride(t *testing.T) {
	t.Setenv("METIS_RESUME_MAX_MB", "16")
	if got := resumeMaxBytes(); got != 16*1024*1024 {
		t.Errorf("env override: got %d, want 16 MiB", got)
	}
	t.Setenv("METIS_RESUME_MAX_MB", "garbage")
	if got := resumeMaxBytes(); got != DefaultResumeMaxBytes {
		t.Errorf("garbage env should fall back to default, got %d", got)
	}
}
