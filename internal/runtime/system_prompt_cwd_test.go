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

// TestLoadProjectContext_HiddenDirVariants — .metis/CLAUDE.md and
// .claude/CLAUDE.md as alternative locations.
func TestLoadProjectContext_HiddenDirVariants(t *testing.T) {
	for _, dir := range []string{".metis", ".claude"} {
		t.Run(dir, func(t *testing.T) {
			tmp := t.TempDir()
			_ = os.MkdirAll(filepath.Join(tmp, dir), 0o755)
			_ = os.WriteFile(filepath.Join(tmp, dir, "CLAUDE.md"), []byte("# hidden body "+dir), 0o644)
			orig, _ := os.Getwd()
			t.Cleanup(func() { _ = os.Chdir(orig) })
			_ = os.Chdir(tmp)

			got := loadProjectContext()
			if !strings.Contains(got, "hidden body "+dir) {
				t.Errorf("%s/CLAUDE.md should load; got %q", dir, got)
			}
		})
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
