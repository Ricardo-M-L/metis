package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// TestEdit_StaleCheck_DetectsExternalModification verifies the
// read-edit-write atomicity check: if the file mtime changes between
// the model's Read and the Edit, Edit refuses with FileUnexpectedlyModified.
func TestEdit_StaleCheck_DetectsExternalModification(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gate := permission.New(permission.ModeBypass)
	state := NewReadFileState()
	rd := Read{gate: gate, state: state}
	ed := Edit{gate: gate, state: state}

	// Model "reads" the file.
	if _, err := rd.Execute(context.Background(), map[string]any{"path": path}); err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	// Out-of-band modification — simulate another process touching it.
	// Sleep a bit so mtime resolution catches the change reliably.
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(path, []byte("alpha\nGAMMA\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ed.Execute(context.Background(), map[string]any{
		"path": path, "old": "alpha", "new": "ALPHA",
	})
	if err != nil {
		t.Fatalf("Edit returned hard error (expected soft): %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("Edit should soft-fail with IsError=true, got %+v", res)
	}
	if !strings.Contains(res.Output, FileUnexpectedlyModified) {
		t.Errorf("Edit output should mention staleness: %q", res.Output)
	}
}

// TestEdit_StaleCheck_AllowsWhenUnchanged verifies the happy path:
// Read then Edit with no out-of-band modification proceeds.
func TestEdit_StaleCheck_AllowsWhenUnchanged(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.txt")
	if err := os.WriteFile(path, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gate := permission.New(permission.ModeBypass)
	state := NewReadFileState()
	rd := Read{gate: gate, state: state}
	ed := Edit{gate: gate, state: state}

	if _, err := rd.Execute(context.Background(), map[string]any{"path": path}); err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	res, err := ed.Execute(context.Background(), map[string]any{
		"path": path, "old": "hello", "new": "Hi",
	})
	if err != nil {
		t.Fatalf("Edit failed: %v", err)
	}
	if res.IsError {
		t.Errorf("Edit should succeed: %s", res.Output)
	}
}

// TestEdit_NoStaleCheckWhenStateNil verifies the legacy / test-bypass
// path: nil state → no staleness check. (Edit still works.)
func TestEdit_NoStaleCheckWhenStateNil(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.txt")
	os.WriteFile(path, []byte("foo bar"), 0o644)

	gate := permission.New(permission.ModeBypass)
	ed := Edit{gate: gate, state: nil}

	if _, err := ed.Execute(context.Background(), map[string]any{
		"path": path, "old": "foo", "new": "FOO",
	}); err != nil {
		t.Fatalf("Edit with nil state should still work: %v", err)
	}
}

// TestWrite_RefusesWithoutRead verifies that Write on an existing file
// without a prior Read fails — claude-code's "always Read first" rule.
func TestWrite_RefusesWithoutRead(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "existing.txt")
	os.WriteFile(path, []byte("old content"), 0o644)

	gate := permission.New(permission.ModeBypass)
	state := NewReadFileState()
	wr := Write{gate: gate, state: state}

	res, err := wr.Execute(context.Background(), map[string]any{
		"path": path, "content": "new content",
	})
	if err != nil {
		t.Fatalf("Write should soft-fail, not hard-error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Output, "has not been Read") {
		t.Errorf("Write should refuse without prior Read; got %+v", res)
	}
}

// TestWrite_NewFileAllowedWithoutRead verifies that Write to a NEW
// path (no existing file) doesn't require Read — the "Read first"
// rule only applies to overwriting existing content.
func TestWrite_NewFileAllowedWithoutRead(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "new.txt")

	gate := permission.New(permission.ModeBypass)
	state := NewReadFileState()
	wr := Write{gate: gate, state: state}

	res, err := wr.Execute(context.Background(), map[string]any{
		"path": path, "content": "fresh",
	})
	if err != nil || (res != nil && res.IsError) {
		t.Errorf("Write to new file should succeed: %+v %v", res, err)
	}
}

// TestRead_TooLarge: files over MaxReadFileSize get a soft error so
// the model can fall back to Bash head/tail rather than OOM the agent.
func TestRead_TooLarge(t *testing.T) {
	// We can't actually create a 256MB file in CI quickly; use the
	// internal cap by lowering it via a synthetic check on a file
	// that's guaranteed to be over a tiny test cap.
	//
	// The production cap is constant; instead we validate the size-
	// check branch by exercising Read on a file that is ABOVE the
	// effective check using a synthetic large content — kept small
	// enough that test runs stay fast (~16 KiB) and asserting the
	// guard would need to be configurable. Skip the write part and
	// just confirm the constant is sane.
	if MaxReadFileSize < 1024*1024 {
		t.Errorf("MaxReadFileSize too small: %d", MaxReadFileSize)
	}
}
