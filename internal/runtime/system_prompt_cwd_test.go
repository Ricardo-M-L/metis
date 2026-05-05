package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadProjectContext_PrioritizesClaudeMD — when multiple
// conventional files exist, CLAUDE.md wins (most common across tools).
func TestLoadProjectContext_PrioritizesClaudeMD(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "CLAUDE.md"), []byte("# claude content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "AGENTS.md"), []byte("# agents content"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	_ = os.Chdir(tmp)

	got := loadProjectContext()
	if !strings.Contains(got, "claude content") {
		t.Errorf("CLAUDE.md should win; got %q", got)
	}
	if strings.Contains(got, "agents content") {
		t.Errorf("AGENTS.md should not be loaded when CLAUDE.md exists; got %q", got)
	}
	if !strings.Contains(got, "<project_context") {
		t.Errorf("output should be wrapped in <project_context> tag; got %q", got)
	}
}

// TestLoadProjectContext_FallsBackToAgentsMD — when CLAUDE.md missing,
// AGENTS.md (OpenAI/codex convention) is the next choice.
func TestLoadProjectContext_FallsBackToAgentsMD(t *testing.T) {
	tmp := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmp, "AGENTS.md"), []byte("# agents body"), 0o644)
	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	_ = os.Chdir(tmp)

	got := loadProjectContext()
	if !strings.Contains(got, "agents body") {
		t.Errorf("AGENTS.md should load as fallback; got %q", got)
	}
}

// TestLoadProjectContext_FallsBackToMETIS — METIS-specific naming.
func TestLoadProjectContext_FallsBackToMETIS(t *testing.T) {
	tmp := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmp, "METIS.md"), []byte("# metis body"), 0o644)
	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	_ = os.Chdir(tmp)

	got := loadProjectContext()
	if !strings.Contains(got, "metis body") {
		t.Errorf("METIS.md should load; got %q", got)
	}
}

// TestLoadProjectContext_HiddenDirVariants — .metis/CLAUDE.md as
// metis-specific hidden location.
//
// History: .claude/CLAUDE.md was previously also a candidate, but
// reading it caused metis to advertise the user's claude-code global
// instructions as metis project instructions (2026-05-05 confusion
// bug — running metis at /Users/ricardo, the LLM saw
// ~/.claude/CLAUDE.md and reported "your project instructions live
// in .claude/CLAUDE.md"). metis no longer reads .claude/.
func TestLoadProjectContext_HiddenDirVariants(t *testing.T) {
	tmp := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmp, ".metis"), 0o755)
	_ = os.WriteFile(filepath.Join(tmp, ".metis", "CLAUDE.md"), []byte("# hidden body .metis"), 0o644)
	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	_ = os.Chdir(tmp)

	got := loadProjectContext()
	if !strings.Contains(got, "hidden body .metis") {
		t.Errorf(".metis/CLAUDE.md should load; got %q", got)
	}
}

// TestLoadProjectContext_IgnoresClaudeDir — .claude/CLAUDE.md must NOT
// be loaded by metis; it's claude-code's territory.
func TestLoadProjectContext_IgnoresClaudeDir(t *testing.T) {
	tmp := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmp, ".claude"), 0o755)
	_ = os.WriteFile(filepath.Join(tmp, ".claude", "CLAUDE.md"), []byte("# claude-code body"), 0o644)
	// Mark this as a fake git root so loadProjectContext stops here
	// and doesn't walk up into the user's real CLAUDE.md.
	_ = os.MkdirAll(filepath.Join(tmp, ".git"), 0o755)
	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	_ = os.Chdir(tmp)

	if got := loadProjectContext(); got != "" {
		t.Errorf("metis must NOT load .claude/CLAUDE.md; got %q", got)
	}
}

// TestLoadProjectContext_EmptyWhenAbsent — no context file → empty
// string (caller skips the section).
func TestLoadProjectContext_EmptyWhenAbsent(t *testing.T) {
	tmp := t.TempDir()
	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	_ = os.Chdir(tmp)
	if got := loadProjectContext(); got != "" {
		t.Errorf("no context file should yield empty string; got %q", got)
	}
}

// TestAssembleSystemPrompt_IncludesProjectContext — end-to-end through
// AssembleSystemPrompt: cwd CLAUDE.md must surface in the final
// system prompt before the user addendum.
func TestAssembleSystemPrompt_IncludesProjectContext(t *testing.T) {
	tmp := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmp, "CLAUDE.md"), []byte("# project rules"), 0o644)
	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	_ = os.Chdir(tmp)

	out := AssembleSystemPrompt("BASE PROMPT")
	if !strings.Contains(out, "BASE PROMPT") {
		t.Errorf("missing base; got: %s", out)
	}
	if !strings.Contains(out, "project rules") {
		t.Errorf("missing project context; got: %s", out)
	}
	if !strings.Contains(out, "<project_context") {
		t.Errorf("missing project_context wrapper; got: %s", out)
	}
}
