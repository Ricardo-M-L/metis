package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAgentProfile_FullFrontmatter(t *testing.T) {
	src := `---
name: reviewer
description: code reviewer
model: claude-haiku-4-5
tools: Read, Glob, Grep
disallowed_tools: Bash, Write
permission_mode: ask
effort: low
max_turns: 20
initial_prompt: review the diff
omit_claude_md: true
---
You are a code reviewer.`
	prof, err := parseAgentProfile(src)
	if err != nil {
		t.Fatal(err)
	}
	if prof.Name != "reviewer" {
		t.Errorf("Name = %q", prof.Name)
	}
	if prof.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q", prof.Model)
	}
	if got := prof.Tools; len(got) != 3 || got[0] != "Read" {
		t.Errorf("Tools = %v", got)
	}
	if got := prof.DisallowedTools; len(got) != 2 || got[1] != "Write" {
		t.Errorf("DisallowedTools = %v", got)
	}
	if prof.PermissionMode != "ask" {
		t.Errorf("PermissionMode = %q", prof.PermissionMode)
	}
	if prof.Effort != "low" {
		t.Errorf("Effort = %q", prof.Effort)
	}
	if prof.MaxTurns != 20 {
		t.Errorf("MaxTurns = %d", prof.MaxTurns)
	}
	if !prof.OmitClaudeMd {
		t.Errorf("OmitClaudeMd should be true")
	}
	if !strings.Contains(prof.SystemPrompt, "code reviewer") {
		t.Errorf("SystemPrompt missing body: %q", prof.SystemPrompt)
	}
}

// TestParseAgentProfile_MemorySnapshot — G.10 (2026-05-12) profile
// frontmatter field. Profile picks up which named snapshot to
// restore before the first turn.
func TestParseAgentProfile_MemorySnapshot(t *testing.T) {
	src := `---
name: researcher
memory_snapshot: research-cluster
---
You are a researcher.`
	prof, err := parseAgentProfile(src)
	if err != nil {
		t.Fatal(err)
	}
	if prof.MemorySnapshot != "research-cluster" {
		t.Errorf("MemorySnapshot = %q, want research-cluster", prof.MemorySnapshot)
	}
}

func TestParseAgentProfile_NoFrontmatter(t *testing.T) {
	src := "Just a body, no frontmatter."
	prof, err := parseAgentProfile(src)
	if err != nil {
		t.Fatal(err)
	}
	if prof.SystemPrompt != "Just a body, no frontmatter." {
		t.Errorf("SystemPrompt = %q", prof.SystemPrompt)
	}
}

func TestLoadAgentProfile_FromUserDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	contents := `---
name: scout
model: claude-haiku-4-5
---
You explore.`
	if err := os.WriteFile(filepath.Join(home, "agents", "scout.md"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	prof, err := LoadAgentProfile("scout")
	if err != nil {
		t.Fatal(err)
	}
	if prof.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q", prof.Model)
	}
}

func TestLoadAgentProfile_MissingErrors(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	_, err := LoadAgentProfile("nonexistent")
	if err == nil {
		t.Errorf("expected error for missing profile")
	}
}

func TestLoadAgentProfile_RejectsBadName(t *testing.T) {
	if _, err := LoadAgentProfile("../etc/passwd"); err == nil {
		t.Errorf("traversal should error")
	}
	if _, err := LoadAgentProfile("name with space"); err == nil {
		t.Errorf("space in name should error")
	}
}

func TestAgentProfile_FilterToolNames(t *testing.T) {
	prof := &AgentProfile{
		Tools:           []string{"Read", "Glob"},
		DisallowedTools: []string{"Glob"},
	}
	got := prof.FilterToolNames([]string{"Read", "Write", "Glob", "Bash"})
	if len(got) != 1 || got[0] != "Read" {
		t.Errorf("filter = %v, want [Read]", got)
	}

	// Nil profile returns input as-is.
	var nilP *AgentProfile
	in := []string{"a", "b"}
	if got := nilP.FilterToolNames(in); len(got) != 2 {
		t.Errorf("nil profile should pass through: %v", got)
	}

	// Empty allowlist + non-empty blocklist.
	prof2 := &AgentProfile{DisallowedTools: []string{"Bash"}}
	if got := prof2.FilterToolNames([]string{"Read", "Bash"}); len(got) != 1 || got[0] != "Read" {
		t.Errorf("blocklist-only filter = %v", got)
	}
}

func TestAgentProfile_MergeOnto(t *testing.T) {
	prof := &AgentProfile{
		Model:          "claude-opus-4-7",
		PermissionMode: "auto",
		Effort:         "high",
		MaxTurns:       50,
	}
	// CLI passes nothing — profile fully wins.
	m, mo, e, it := prof.MergeOnto("", "", "", 0)
	if m != "claude-opus-4-7" || mo != "auto" || e != "high" || it != 50 {
		t.Errorf("profile-only merge: got (%q,%q,%q,%d)", m, mo, e, it)
	}

	// CLI overrides model — profile loses on that field only.
	m2, mo2, _, _ := prof.MergeOnto("claude-haiku-4-5", "", "", 0)
	if m2 != "claude-haiku-4-5" || mo2 != "auto" {
		t.Errorf("CLI override broke: got (%q,%q)", m2, mo2)
	}
}
