package tui

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

func writeFakeGit(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "git")
	script := "#!/bin/sh\nset -eu\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestDiffCancellationHelperProcess(t *testing.T) {
	switch os.Getenv("METIS_DIFF_CANCELLATION_HELPER") {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestDiffCancellationHelperProcess$")
		child.Env = append(os.Environ(), "METIS_DIFF_CANCELLATION_HELPER=child")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		time.Sleep(5 * time.Second)
		os.Exit(0)
	case "child":
		// The child deliberately outlives its parent and keeps the inherited
		// stdout/stderr pipes open. Killing only the parent therefore makes
		// exec.Cmd.Wait block until this sleep finishes.
		time.Sleep(5 * time.Second)
		os.Exit(0)
	}
}

func TestDiffCommandReturnsTypedResultWithoutMutatingModel(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/diff-view")
	cmd := pressEnter(t, m)
	if cmd == nil || !m.diffPending || m.activeScreen != nil {
		t.Fatalf("async start = cmd:%v pending:%v screen:%T", cmd != nil, m.diffPending, m.activeScreen)
	}
	result, ok := cmd().(diffResultMsg)
	if !ok {
		t.Fatalf("command result type = %T", cmd())
	}
	if !m.diffPending || m.activeScreen != nil {
		t.Fatalf("command goroutine mutated model: pending:%v screen:%T", m.diffPending, m.activeScreen)
	}
	m.Update(result)
	if m.diffPending || m.activeScreen == nil {
		t.Fatalf("Update did not apply result: pending:%v screen:%T", m.diffPending, m.activeScreen)
	}
}

func TestDiffDuplicateTriggerIgnoredWhilePending(t *testing.T) {
	m := newSlashTestModel(t)
	first := m.openDiffViewer()
	second := m.openDiffViewer()
	if first == nil || second != nil {
		t.Fatalf("duplicate commands = first:%v second:%v", first != nil, second != nil)
	}
	if !strings.Contains(m.messages[len(m.messages)-1].Content, "already in progress") {
		t.Fatalf("duplicate status missing: %+v", m.messages[len(m.messages)-1])
	}
}

func TestDiffStaleResultIgnoredAfterSessionChange(t *testing.T) {
	m := newSlashTestModel(t)
	cmd := m.openDiffViewer()
	msg := cmd().(diffResultMsg)
	m.sessionID = "different-session"
	m.Update(msg)
	if m.activeScreen != nil || m.diffPending {
		t.Fatalf("stale result applied: pending:%v screen:%T", m.diffPending, m.activeScreen)
	}
	if !strings.Contains(m.messages[len(m.messages)-1].Content, "active session changed") {
		t.Fatalf("stale status missing: %+v", m.messages[len(m.messages)-1])
	}
}

func TestRunBoundedDiffCommandCancelsSlowGit(t *testing.T) {
	t.Setenv("METIS_DIFF_CANCELLATION_HELPER", "parent")
	t.Setenv("METIS_DIFF_CANCELLATION_HELPER_BINARY", os.Args[0])
	writeFakeGit(t, `exec "$METIS_DIFF_CANCELLATION_HELPER_BINARY" -test.run=^TestDiffCancellationHelperProcess$`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runBoundedDiffCommand(ctx, "", 1024, "diff")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow git error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("slow git cancellation took %s", elapsed)
	}
}

func TestCanceledDiffOpensExplicitErrorInsteadOfCleanState(t *testing.T) {
	m := newSlashTestModel(t)
	m.diffPending = true
	m.diffSeq = 9
	m.handleDiffResult(diffResultMsg{
		requestID: 9,
		loop:      m.loop,
		sessionID: m.sessionID,
		sources: []screen.DiffSource{{
			Label: "Working tree", Error: diffContextStatus(context.Canceled),
		}},
	})
	if m.activeScreen == nil {
		t.Fatal("canceled result did not create an explicit status screen")
	}
	view := m.activeScreen.View()
	if !strings.Contains(view, "diff collection canceled") || strings.Contains(view, "Working tree clean") {
		t.Fatalf("canceled result was presented as clean:\n%s", view)
	}
}

func TestTimedOutDiffOpensExplicitErrorInsteadOfCleanState(t *testing.T) {
	writeFakeGit(t, `sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	sources := collectDiffSources(ctx, nil)
	if len(sources) != 1 || !strings.Contains(sources[0].Error, "timed out") || len(sources[0].Files) != 0 {
		t.Fatalf("timeout sources = %+v", sources)
	}
	viewer := screen.NewDiffViewerScreenWithSources(sources)
	viewer.Resize(100, 30)
	if view := viewer.View(); !strings.Contains(view, "timed out") || strings.Contains(view, "Working tree clean") {
		t.Fatalf("timeout result was presented as clean:\n%s", view)
	}
}

func TestRunBoundedDiffCommandCapsOutput(t *testing.T) {
	writeFakeGit(t, `yes x | head -c 65536`)
	out, err := runBoundedDiffCommand(context.Background(), "", 1024, "diff")
	if !errors.Is(err, errDiffOutputLimit) {
		t.Fatalf("large output error = %v, want output limit", err)
	}
	if len(out) != 1024 {
		t.Fatalf("captured bytes = %d, want 1024", len(out))
	}
}

func TestWorkingTreeDiffSkipsOversizeUntrackedWithExplicitStatus(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "huge.txt"), make([]byte, diffMaxFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	result := buildWorkingTreeDiffFilesContext(context.Background())
	if result.status != "" || !result.truncated || len(result.files) != 0 {
		t.Fatalf("oversize result = %+v", result)
	}
	sources := collectDiffSources(context.Background(), nil)
	if len(sources) == 0 || !strings.Contains(sources[0].Subtitle, "truncated") || sources[0].Error != "" {
		t.Fatalf("oversize display status = %+v", sources)
	}
}

func TestBuildTurnDiffSourcesReconstructsEditsNewestFirst(t *testing.T) {
	history := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "first change"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUseID: "edit-1", ToolName: "Edit",
			ToolInput: map[string]any{"path": "first.go", "old": "old\n", "new": "new\n"},
		}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "edit-1", ToolResult: "ok"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "second change"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUseID: "write-2", ToolName: "Write",
			ToolInput: map[string]any{"path": "second.go", "content": "package second\n"},
		}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "write-2", ToolResult: "ok"}}},
	}

	sources := buildTurnDiffSources(history)
	if len(sources) != 2 {
		t.Fatalf("sources = %d, want 2: %+v", len(sources), sources)
	}
	if sources[0].Label != "Turn 2" || sources[0].Files[0].Path != "second.go" || sources[0].Files[0].Status != "A" {
		t.Fatalf("newest source = %+v", sources[0])
	}
	if sources[1].Label != "Turn 1" || sources[1].Files[0].Path != "first.go" {
		t.Fatalf("oldest source = %+v", sources[1])
	}
	viewPatch := sources[1].Files[0].Hunks[0]
	if len(viewPatch.Lines) < 2 || viewPatch.Lines[0].Type != "-" || viewPatch.Lines[1].Type != "+" {
		t.Fatalf("edit hunk not reconstructed: %+v", viewPatch)
	}
	if !strings.Contains(sources[0].Subtitle, "second change") {
		t.Fatalf("turn prompt missing: %+v", sources[0])
	}
	if !strings.Contains(strings.ToLower(sources[0].Subtitle), "reconstructed") ||
		!strings.Contains(strings.ToLower(sources[0].Subtitle), "intent") {
		t.Fatalf("fallback source did not disclose reconstruction limits: %+v", sources[0])
	}
}

func TestBuildWorkingTreeDiffFilesIncludesUnbornRepositoryFiles(t *testing.T) {
	repo := t.TempDir()
	command := exec.Command("git", "init", "-q")
	command.Dir = repo
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "first file.go"), []byte("package first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalWD) })

	files, err := buildWorkingTreeDiffFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "first file.go" || files[0].Status != "A" {
		t.Fatalf("unborn working tree files = %+v", files)
	}
}

func TestBuildWorkingTreeDiffFilesReportsNonGitDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	files, err := buildWorkingTreeDiffFiles()
	if err == nil {
		t.Fatalf("non-git collector returned clean state: %+v", files)
	}
}

func TestBuildTurnDiffSourcesOmitsFailedEdits(t *testing.T) {
	history := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "bad edit"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUseID: "failed", ToolName: "Edit",
			ToolInput: map[string]any{"path": "bad.go", "old": "a", "new": "b"},
		}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "failed", ToolResult: "no", IsError: true}}},
	}
	if got := buildTurnDiffSources(history); len(got) != 0 {
		t.Fatalf("failed edit became a source: %+v", got)
	}
}

func TestParseViewerPatchPreservesPathWithSpaces(t *testing.T) {
	patch := "diff --git a/folder/new file.go b/folder/new file.go\n" +
		"--- a/folder/new file.go\n+++ b/folder/new file.go\n" +
		"@@ -1 +1 @@\n-old\n+new\n"
	files := parseViewerPatch(patch)
	if len(files) != 1 || files[0].Path != "folder/new file.go" {
		t.Fatalf("files = %+v", files)
	}
}

func TestDiffSourceSubtitleDisclosesIntervalAndSanitizesPrompt(t *testing.T) {
	got := diffSourceSubtitle("checkpoint interval (best effort)", "prompt\x1b[2Jspoof")
	if !strings.Contains(got, "checkpoint interval (best effort)") ||
		!strings.Contains(got, "[private]") || strings.ContainsRune(got, '\x1b') {
		t.Fatalf("unsafe or misleading checkpoint subtitle: %q", got)
	}
	if len([]rune(got)) > 72 {
		t.Fatalf("checkpoint subtitle is unbounded: %q", got)
	}
}

func TestCanonicalDiffOpensInteractiveViewer(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/diff")
	cmd := pressEnter(t, m)
	if cmd == nil {
		t.Fatal("/diff returned no async command; collection ran inside Update")
	}
	if m.activeScreen != nil {
		t.Fatalf("/diff opened %T before the async result reached Update", m.activeScreen)
	}
	msg := cmd()
	if m.activeScreen != nil {
		t.Fatalf("diff command mutated the model off the Update goroutine: %T", m.activeScreen)
	}
	m.Update(msg)
	if m.activeScreen == nil {
		t.Fatal("/diff result did not open an interactive screen")
	}
	view := m.activeScreen.View()
	if !strings.Contains(view, "Working tree") || !strings.Contains(view, "source") {
		t.Fatalf("/diff opened the wrong surface:\n%s", view)
	}
}

func TestPlainREPLDiffRequiresTUIWhileGitDiffKeepsRawOutput(t *testing.T) {
	repo := t.TempDir()
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalWD) })

	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = repo
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	const marker = "metis-plain-diff-marker"
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	git("-c", "user.name=Metis Test", "-c", "user.email=metis@example.invalid", "commit", "-qm", "base")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte(marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	registry := slash.NewRegistry()
	slash.RegisterAll(registry, &config.Config{})
	r, out := newPromptTestREPL("/diff\n/git diff\n/quit\n", registry)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	stdout := stripANSI(out.String())
	if count := strings.Count(stdout, marker); count != 1 {
		t.Fatalf("raw patch marker count = %d, want exactly one from /git diff:\n%s", count, stdout)
	}
	if !strings.Contains(stdout, "/git diff") || !strings.Contains(stdout, "interactive TUI") {
		t.Fatalf("plain /diff did not explain the interactive/raw split:\n%s", stdout)
	}
}
