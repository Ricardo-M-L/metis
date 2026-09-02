package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteMissingContentCannotCreateOrTruncateFile(t *testing.T) {
	dir := t.TempDir()
	newPath := filepath.Join(dir, "new.txt")
	res, err := (Write{}).Execute(context.Background(), map[string]any{"path": newPath})
	if err != nil {
		t.Fatalf("Write.Execute: %v", err)
	}
	if res == nil || !res.IsError || !strings.Contains(res.Output, "content") {
		t.Fatalf("result = %+v, want content validation error", res)
	}
	if _, statErr := os.Stat(newPath); !os.IsNotExist(statErr) {
		t.Fatalf("missing-content Write created a file: stat err=%v", statErr)
	}

	existing := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(existing, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = (Write{}).Execute(context.Background(), map[string]any{"path": existing, "content": 7})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("wrong-type content result=%+v err=%v", res, err)
	}
	got, readErr := os.ReadFile(existing)
	if readErr != nil || string(got) != "keep\n" {
		t.Fatalf("wrong-type content changed file: data=%q err=%v", got, readErr)
	}
}

func TestWriteExplicitEmptyContentRemainsValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	res, err := (Write{}).Execute(context.Background(), map[string]any{"path": path, "content": ""})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("explicit empty Write result=%+v err=%v", res, err)
	}
	if got, err := os.ReadFile(path); err != nil || len(got) != 0 {
		t.Fatalf("empty file = %q err=%v", got, err)
	}
}

func TestEditMissingNewCannotBecomeDeletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edit.txt")
	const original = "keep this text\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := (Edit{}).Execute(context.Background(), map[string]any{
		"path": path,
		"old":  "keep ",
	})
	if err != nil {
		t.Fatalf("Edit.Execute: %v", err)
	}
	if res == nil || !res.IsError || !strings.Contains(res.Output, "new") {
		t.Fatalf("result = %+v, want new validation error", res)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != original {
		t.Fatalf("missing-new Edit changed file: data=%q err=%v", got, readErr)
	}
}

func TestEditExplicitEmptyNewRemainsValidDeletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delete.txt")
	if err := os.WriteFile(path, []byte("remove keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := (Edit{}).Execute(context.Background(), map[string]any{
		"path": path,
		"old":  "remove ",
		"new":  "",
	})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("explicit deletion result=%+v err=%v", res, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "keep\n" {
		t.Fatalf("deletion output=%q err=%v", got, err)
	}
}
