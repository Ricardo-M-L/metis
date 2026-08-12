package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestParseSessionListOptions(t *testing.T) {
	workDir := t.TempDir()
	got, err := parseSessionListOptions([]string{"--json", "--work-dir", workDir, "-n", "30"})
	if err != nil {
		t.Fatal(err)
	}
	want := sessionListOptions{limit: 30, jsonOutput: true, workDir: workDir}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("options = %#v, want %#v", got, want)
	}
	for _, args := range [][]string{
		{"--work-dir"}, {"--work-dir", ""}, {"--limit"}, {"--limit", "0"}, {"--unknown"},
	} {
		if _, err := parseSessionListOptions(args); err == nil {
			t.Errorf("parseSessionListOptions(%#v) unexpectedly succeeded", args)
		}
	}
}

func TestListSessionEntriesFiltersWorkspaceBeforeLimit(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	currentWorkDir := filepath.Join(t.TempDir(), "current")
	otherWorkDir := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(currentWorkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(otherWorkDir, 0o755); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	if err := store.WriteHeaderFull(session.Header{
		ID: "current-old", WorkDir: currentWorkDir, Model: "current-model", CreatedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("current-old", llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "current"}}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 31; i++ {
		id := fmt.Sprintf("other-new-%02d", i)
		if err := store.WriteHeaderFull(session.Header{
			ID:        id,
			WorkDir:   otherWorkDir,
			Model:     "other-model",
			CreatedAt: base.Add(time.Duration(i+1) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessage(id, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "other"}}}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := listSessionEntries(store, sessionListOptions{limit: 30, workDir: currentWorkDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "current-old" {
		t.Fatalf("filtered sessions = %#v, want current-old", got)
	}
}

func TestListSessionEntriesIncludesLegacyHeaderWithoutWorkDir(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeaderFull(session.Header{ID: "legacy", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("legacy", llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "legacy"}}}); err != nil {
		t.Fatal(err)
	}
	got, err := listSessionEntries(store, sessionListOptions{limit: 30, workDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "legacy" {
		t.Fatalf("legacy sessions = %#v", got)
	}
}

func TestSameSessionWorkDirResolvesSymlink(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	realDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if !sameSessionWorkDir(realDir, linkDir) {
		t.Fatalf("real path %q and symlink %q should match", realDir, linkDir)
	}
	if sameSessionWorkDir(realDir, filepath.Join(t.TempDir(), strings.Repeat("x", 2))) {
		t.Fatal("different paths unexpectedly matched")
	}
}
