package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
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
permission_mode: default
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
	if prof.PermissionMode != "default" {
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
	t.Chdir(t.TempDir())
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
	if prof.Source != AgentProfileSourceUser {
		t.Errorf("Source = %q, want user", prof.Source)
	}
}

func TestLoadAgentProfileRecordsProjectAndBuiltinProvenance(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(project)
	projectAgents := filepath.Join(project, ".metis", "agents")
	if err := os.MkdirAll(projectAgents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectAgents, "explore.md"), []byte("Project override"), 0o644); err != nil {
		t.Fatal(err)
	}

	projectProfile, err := LoadAgentProfile("explore")
	if err != nil {
		t.Fatal(err)
	}
	if projectProfile.Source != AgentProfileSourceProject {
		t.Fatalf("project profile source = %q, want project", projectProfile.Source)
	}
	if err := os.Remove(filepath.Join(projectAgents, "explore.md")); err != nil {
		t.Fatal(err)
	}
	builtinProfile, err := LoadAgentProfile("explore")
	if err != nil {
		t.Fatal(err)
	}
	if builtinProfile.Source != AgentProfileSourceBuiltin {
		t.Fatalf("bundled profile source = %q, want builtin", builtinProfile.Source)
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

func TestAvailableAgentProfileNamesMergesBuiltinProjectAndUser(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(project)

	projectAgents := filepath.Join(project, ".metis", "agents")
	userAgents := filepath.Join(home, "agents")
	for _, dir := range []string{projectAgents, userAgents} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(projectAgents, "project-scout.md"): "Project scout",
		filepath.Join(projectAgents, "explore.md"):       "Project override",
		filepath.Join(projectAgents, "ignored.txt"):      "not a profile",
		filepath.Join(projectAgents, "bad name.md"):      "invalid slug",
		filepath.Join(userAgents, "user-auditor.md"):     "User auditor",
		filepath.Join(userAgents, "verify.md"):           "User override",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := AvailableAgentProfileNames()
	for _, want := range []string{"explore", "general", "project-scout", "user-auditor", "verify"} {
		if !slices.Contains(got, want) {
			t.Fatalf("available profiles %v missing %q", got, want)
		}
	}
	for _, unwanted := range []string{"research", "ignored", "bad name"} {
		if slices.Contains(got, unwanted) {
			t.Fatalf("available profiles %v unexpectedly contain %q", got, unwanted)
		}
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("available profiles are not sorted: %v", got)
	}
	seen := make(map[string]struct{}, len(got))
	for _, name := range got {
		if _, duplicate := seen[name]; duplicate {
			t.Fatalf("available profiles contain duplicate %q: %v", name, got)
		}
		seen[name] = struct{}{}
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
		PermissionMode: "acceptEdits",
		Effort:         "high",
		MaxTurns:       50,
	}
	// CLI passes nothing — profile fully wins.
	m, mo, e, it := prof.MergeOnto("", "", "", 0)
	if m != "claude-opus-4-7" || mo != "acceptEdits" || e != "high" || it != 50 {
		t.Errorf("profile-only merge: got (%q,%q,%q,%d)", m, mo, e, it)
	}

	// CLI overrides model — profile loses on that field only.
	m2, mo2, _, _ := prof.MergeOnto("claude-haiku-4-5", "", "", 0)
	if m2 != "claude-haiku-4-5" || mo2 != "acceptEdits" {
		t.Errorf("CLI override broke: got (%q,%q)", m2, mo2)
	}
}
