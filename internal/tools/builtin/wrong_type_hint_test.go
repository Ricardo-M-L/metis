package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLS_OnRegularFile_HintsRead — the 2026-05-16 longrun produced
// four "[tool error] open .../provider.go: not a directory" lines
// because the model called LS on a file path. Without the hint,
// each retry burned ~10K tokens of reasoning. We surface the
// Read-tool pointer so a single tool round-trip is enough.
func TestLS_OnRegularFile_HintsRead(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "provider.go")
	if err := os.WriteFile(file, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := LS{gate: bypassGate()}.Execute(context.Background(), map[string]any{"path": file})
	if err != nil {
		t.Fatalf("expected friendly Result, got error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true")
	}
	if !strings.Contains(res.Output, "Read") {
		t.Errorf("LS-on-file error must point to Read tool; got: %q", res.Output)
	}
	if !strings.Contains(res.Output, "regular file") {
		t.Errorf("LS-on-file error must explain the type mismatch; got: %q", res.Output)
	}
}

// TestRead_OnDirectory_HintsLS — symmetric: a model that passes a
// directory to Read should get LS as the pointer rather than the
// raw "is a directory" syscall message.
func TestRead_OnDirectory_HintsLS(t *testing.T) {
	dir := t.TempDir()
	res, err := Read{}.Execute(context.Background(), map[string]any{"path": dir})
	if err != nil {
		t.Fatalf("expected friendly Result, got error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true")
	}
	if !strings.Contains(res.Output, "LS") {
		t.Errorf("Read-on-dir error must point to LS tool; got: %q", res.Output)
	}
	if !strings.Contains(res.Output, "directory") {
		t.Errorf("Read-on-dir error must explain the type mismatch; got: %q", res.Output)
	}
}

// TestEdit_OnDirectory_HintsFilePath — Edit gets the same treatment:
// directory input → friendly Result, not a confusing syscall error.
func TestEdit_OnDirectory_HintsFilePath(t *testing.T) {
	dir := t.TempDir()
	res, err := Edit{}.Execute(context.Background(), map[string]any{
		"path": dir,
		"old":  "x",
		"new":  "y",
	})
	if err != nil {
		t.Fatalf("expected friendly Result, got error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true")
	}
	if !strings.Contains(res.Output, "directory") {
		t.Errorf("Edit-on-dir error must explain the type mismatch; got: %q", res.Output)
	}
}

// TestWrite_OnDirectory_HintsFilename — same for Write. Without this,
// the user-facing error would be the underlying os.WriteFile syscall
// message which is even less actionable than the others.
func TestWrite_OnDirectory_HintsFilename(t *testing.T) {
	dir := t.TempDir()
	res, err := Write{}.Execute(context.Background(), map[string]any{
		"path":    dir,
		"content": "x",
	})
	if err != nil {
		t.Fatalf("expected friendly Result, got error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true")
	}
	if !strings.Contains(res.Output, "directory") || !strings.Contains(res.Output, "filename") {
		t.Errorf("Write-on-dir error must explain the type mismatch and suggest a filename; got: %q", res.Output)
	}
}
