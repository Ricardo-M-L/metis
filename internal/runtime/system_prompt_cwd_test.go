package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadProjectContext_LoadsBothWhenBothExist — 2026-05-09 change
// (#9 SUMMARY): when CLAUDE.md AND AGENTS.md coexist in the same dir,
// the loader emits BOTH so a hand-written AGENTS.md doesn't silently
// shadow CLAUDE.md (or vice-versa). Order-within-dir is the
// projectContextCandidates list (CLAUDE.md first, AGENTS.md second).
func TestLoadProjectContext_LoadsBothWhenBothExist(t *testing.T) {
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
		t.Errorf("CLAUDE.md body missing; got %q", got)
	}
	if !strings.Contains(got, "agents content") {
		t.Errorf("AGENTS.md body missing; got %q", got)
	}
	// CLAUDE.md is first in projectContextCandidates so it must
	// appear before AGENTS.md in the rendered output.
	idxC := strings.Index(got, "claude content")
	idxA := strings.Index(got, "agents content")
	if idxC > idxA {
		t.Errorf("CLAUDE.md should precede AGENTS.md in output; got C@%d A@%d", idxC, idxA)
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
