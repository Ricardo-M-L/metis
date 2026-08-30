package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// TestReadFileState_RecordsAndGets covers the basic round-trip:
// Record then Get returns the entry with default (full) view fields.
func TestReadFileState_RecordsAndGets(t *testing.T) {
	s := NewReadFileState()
	s.Record("/a", time.Unix(1000, 0), []byte("hello"))

	e, ok := s.Get("/a")
	if !ok {
		t.Fatal("Get returned ok=false after Record")
	}
	if e.IsPartialView {
		t.Error("default Record should produce full view (IsPartialView=false)")
	}
	if e.Size != 5 {
		t.Errorf("Size: want 5, got %d", e.Size)
	}
	if e.Hash == "" {
		t.Error("Hash should be populated")
	}
	if e.Offset != 1 {
		t.Errorf("Offset for full view should be 1, got %d", e.Offset)
	}
}

// TestReadFileState_RecordPartial flags partial views correctly.
func TestReadFileState_RecordPartial(t *testing.T) {
	s := NewReadFileState()
	s.RecordPartial("/a", time.Unix(1000, 0), []byte("full content"), 50, 100)

	e, ok := s.Get("/a")
	if !ok {
		t.Fatal("Get failed after RecordPartial")
	}
	if !e.IsPartialView {
		t.Error("IsPartialView should be true after RecordPartial")
	}
	if e.Offset != 50 || e.Limit != 100 {
		t.Errorf("Offset/Limit not preserved: got %d/%d, want 50/100", e.Offset, e.Limit)
	}
}

// TestReadFileState_LRUEvicts100thOldest verifies the cap.
func TestReadFileState_LRUEvicts100thOldest(t *testing.T) {
	s := NewReadFileState()
	for i := 0; i < ReadStateMaxEntries; i++ {
		s.Record(fmt.Sprintf("/p%d", i), time.Now(), []byte("x"))
	}
	if s.Len() != ReadStateMaxEntries {
		t.Fatalf("Len after %d records: want %d, got %d", ReadStateMaxEntries, ReadStateMaxEntries, s.Len())
	}

	// One more push triggers eviction of oldest (path "/p0").
	s.Record("/extra", time.Now(), []byte("y"))
	if s.Len() != ReadStateMaxEntries {
		t.Errorf("Len after eviction: want %d, got %d", ReadStateMaxEntries, s.Len())
	}
	if _, ok := s.Get("/p0"); ok {
		t.Error("oldest entry /p0 should have been evicted")
	}
	if _, ok := s.Get("/extra"); !ok {
		t.Error("just-recorded /extra should still be present")
	}
}

// TestReadFileState_RecordingExistingPathReorders ensures the LRU
// move-to-front works: re-recording an old path should keep it from
// being evicted on the next overflow.
func TestReadFileState_RecordingExistingPathReorders(t *testing.T) {
	s := NewReadFileState()
	for i := 0; i < ReadStateMaxEntries; i++ {
		s.Record(fmt.Sprintf("/p%d", i), time.Now(), []byte("x"))
	}
	// Touch the oldest so it's now the newest.
	s.Record("/p0", time.Now(), []byte("refreshed"))
	// Now overflow — the new oldest should be /p1, not /p0.
	s.Record("/extra", time.Now(), []byte("y"))

	if _, ok := s.Get("/p0"); !ok {
		t.Error("/p0 should still be present after touch")
	}
	if _, ok := s.Get("/p1"); ok {
		t.Error("/p1 should have been evicted as new oldest")
	}
}

// TestReadFileState_Reset clears all entries.
func TestReadFileState_Reset(t *testing.T) {
	s := NewReadFileState()
	s.Record("/a", time.Now(), []byte("x"))
	s.Reset()
	if s.Len() != 0 {
		t.Errorf("Len after Reset: want 0, got %d", s.Len())
	}
	if _, ok := s.Get("/a"); ok {
		t.Error("entries should be gone after Reset")
	}
}

// TestReadFileState_NilSafe — all methods tolerate a nil receiver
// (test/legacy pathway).
func TestReadFileState_NilSafe(t *testing.T) {
	var s *ReadFileState
	s.Record("/a", time.Now(), []byte("x"))
	s.RecordPartial("/a", time.Now(), []byte("x"), 1, 1)
	if _, ok := s.Get("/a"); ok {
		t.Error("nil store should never return ok")
	}
	if s.Len() != 0 {
		t.Error("nil store Len should be 0")
	}
	s.Reset()
}

// TestRead_RecordsPartialOnLimit verifies the read path: when a Read
// hits its limit, the entry is flagged IsPartialView so the next
// Edit/Write can refuse the write.
func TestRead_RecordsPartialOnLimit(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "long.txt")
	var sb strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	gate := permission.New(permission.ModeBypass)
	state := NewReadFileState()
	rd := Read{gate: gate, state: state}

	// Read with limit=10 → forced partial view.
	if _, err := rd.Execute(context.Background(), map[string]any{
		"path":  path,
		"limit": 10,
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	e, ok := state.Get(path)
	if !ok {
		t.Fatal("Read should record the file")
	}
	if !e.IsPartialView {
		t.Error("Read with limit=10 on a 50-line file should flag IsPartialView")
	}
}

// TestRead_RecordsFullOnSmallFile verifies the inverse: a Read whose
// limit is not hit produces a full-view entry.
func TestRead_RecordsFullOnSmallFile(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "short.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gate := permission.New(permission.ModeBypass)
	state := NewReadFileState()
	rd := Read{gate: gate, state: state}

	if _, err := rd.Execute(context.Background(), map[string]any{
		"path":  path,
		"limit": 100, // larger than file
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	e, ok := state.Get(path)
	if !ok {
		t.Fatal("Read should record the file")
	}
	if e.IsPartialView {
		t.Error("Read with limit > file lines should NOT flag IsPartialView")
	}
}

func TestRead_ExactlyLimitLinesIsFullButLimitPlusOneIsPartial(t *testing.T) {
	for _, tc := range []struct {
		name        string
		content     string
		wantPartial bool
	}{
		{name: "exactly limit", content: "one\ntwo\nthree\n", wantPartial: false},
		{name: "limit plus one", content: "one\ntwo\nthree\nfour\n", wantPartial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lines.txt")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			state := NewReadFileState()
			result, err := (Read{gate: permission.New(permission.ModeBypassPermissions), state: state}).Execute(
				context.Background(), map[string]any{"path": path, "limit": 3})
			if err != nil || result == nil || result.IsError {
				t.Fatalf("Read = %+v, %v", result, err)
			}
			entry, ok := state.Get(path)
			if !ok || entry.IsPartialView != tc.wantPartial {
				t.Fatalf("entry = %+v, %v; want partial=%v", entry, ok, tc.wantPartial)
			}
		})
	}
}

// TestEdit_AllowsTargetedEditAfterPartialView pins the Codex-style targeted
// patch contract: a unique old string may be replaced after a partial Read.
// Read captured the full-file hash internally, so stale external changes are
// still rejected while untouched bytes remain byte-for-byte identical.
func TestEdit_AllowsTargetedEditAfterPartialView(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "long.txt")
	var sb strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	gate := permission.New(permission.ModeBypass)
	state := NewReadFileState()
	rd := Read{gate: gate, state: state}
	ed := Edit{gate: gate, state: state}

	// Partial Read.
	if _, err := rd.Execute(context.Background(), map[string]any{
		"path":  path,
		"limit": 5,
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}

	res, err := ed.Execute(context.Background(), map[string]any{
		"path": path, "old": "line 1\n", "new": "LINE 1\n",
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res == nil || res.IsError {
		t.Fatalf("targeted Edit should succeed after partial view, got %+v", res)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "LINE 1\nline 2\n") || !strings.HasSuffix(string(got), "line 50\n") {
		t.Fatalf("targeted Edit changed unexpected bytes:\n%s", got)
	}
}

func TestEdit_PartialViewStillRejectsExternalChange(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "long.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModeBypass)
	state := NewReadFileState()
	rd := Read{gate: gate, state: state}
	ed := Edit{gate: gate, state: state}
	if _, err := rd.Execute(context.Background(), map[string]any{"path": path, "limit": 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("alpha\nbeta changed externally\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := ed.Execute(context.Background(), map[string]any{
		"path": path, "old": "alpha", "new": "ALPHA",
	})
	if err != nil {
		t.Fatalf("Edit hard-errored, expected stale soft failure: %v", err)
	}
	if res == nil || !res.IsError || !strings.Contains(res.Output, FileUnexpectedlyModified) {
		t.Fatalf("partial-view Edit ignored external change: %+v", res)
	}
}

// TestEdit_AllowsAfterFullReread: the model can recover by re-Reading
// without offset/limit, which replaces the partial entry with a full
// one and unblocks Edit.
func TestEdit_AllowsAfterFullReread(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "long.txt")
	var sb strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	gate := permission.New(permission.ModeBypass)
	state := NewReadFileState()
	rd := Read{gate: gate, state: state}
	ed := Edit{gate: gate, state: state}

	// Partial first.
	rd.Execute(context.Background(), map[string]any{"path": path, "limit": 5})
	// Then full re-read with a generous limit.
	if _, err := rd.Execute(context.Background(), map[string]any{
		"path":  path,
		"limit": 5000,
	}); err != nil {
		t.Fatalf("Re-read: %v", err)
	}

	// "line 1\n" is unique (line 10..19 don't end at 1+\n).
	res, err := ed.Execute(context.Background(), map[string]any{
		"path": path, "old": "line 1\n", "new": "LINE 1\n",
	})
	if err != nil {
		t.Fatalf("Edit failed: %v", err)
	}
	if res.IsError {
		t.Errorf("Edit should succeed after full re-read: %s", res.Output)
	}
}

// TestWrite_RefusesPartialView mirrors TestEdit_RefusesPartialView for
// Write — same safety property.
func TestWrite_RefusesPartialView(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "long.txt")
	var sb strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	gate := permission.New(permission.ModeBypass)
	state := NewReadFileState()
	rd := Read{gate: gate, state: state}
	wr := Write{gate: gate, state: state}

	if _, err := rd.Execute(context.Background(), map[string]any{
		"path":  path,
		"limit": 5,
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}

	res, err := wr.Execute(context.Background(), map[string]any{
		"path":    path,
		"content": "rewritten\n",
	})
	if err != nil {
		t.Fatalf("Write hard-errored: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("Write should soft-fail on partial-view entry, got %+v", res)
	}
	if !strings.Contains(res.Output, "partial view") {
		t.Errorf("Write error should mention partial view: %q", res.Output)
	}
}

// TestRead_OffsetTriggersPartial — offset != 1 always means partial,
// regardless of how many lines are returned. The model never saw the
// pre-offset region.
func TestRead_OffsetTriggersPartial(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gate := permission.New(permission.ModeBypass)
	state := NewReadFileState()
	rd := Read{gate: gate, state: state}

	if _, err := rd.Execute(context.Background(), map[string]any{
		"path":   path,
		"offset": 2,
		"limit":  5000,
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	e, ok := state.Get(path)
	if !ok {
		t.Fatal("Read should record")
	}
	if !e.IsPartialView {
		t.Error("offset != 1 should always trigger IsPartialView")
	}
}
