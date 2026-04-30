package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitFrontmatter_Basic(t *testing.T) {
	in := []byte("---\nname: foo\n---\nbody here\n")
	header, body, ok := splitFrontmatter(in)
	if !ok {
		t.Fatal("expected frontmatter detected")
	}
	if !strings.Contains(string(header), "name: foo") {
		t.Errorf("header = %q", header)
	}
	if string(body) != "body here\n" {
		t.Errorf("body = %q", body)
	}
}

func TestSplitFrontmatter_NoFrontmatter(t *testing.T) {
	in := []byte("just a body\nno fence\n")
	header, body, ok := splitFrontmatter(in)
	if ok {
		t.Error("expected ok=false for no frontmatter")
	}
	if header != nil {
		t.Errorf("header should be nil; got %q", header)
	}
	if string(body) != string(in) {
		t.Errorf("body should be entire content; got %q", body)
	}
}

func TestSplitFrontmatter_CRLF(t *testing.T) {
	in := []byte("---\r\nname: foo\r\n---\r\nbody\r\n")
	_, body, ok := splitFrontmatter(in)
	if !ok {
		t.Fatal("CRLF frontmatter should still be detected")
	}
	if !strings.Contains(string(body), "body") {
		t.Errorf("body = %q", body)
	}
}

func TestParseMarkdown_FullManifest(t *testing.T) {
	in := []byte(`---
name: code-review
description: Review staged diff
when_to_use: User says "review this PR"
allowed_tools: [Read, Bash, Grep]
model_override: claude-opus-4-7
version: 1.2.0
tags: [workflow, review]
---
You are a senior code reviewer.
Walk the diff and flag issues.
`)
	sk, err := parseMarkdown(in, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "code-review" {
		t.Errorf("Name = %q", sk.Name)
	}
	if sk.Description != "Review staged diff" {
		t.Errorf("Description = %q", sk.Description)
	}
	if len(sk.AllowedTools) != 3 || sk.AllowedTools[0] != "Read" {
		t.Errorf("AllowedTools = %v", sk.AllowedTools)
	}
	if sk.ModelOverride != "claude-opus-4-7" {
		t.Errorf("ModelOverride = %q", sk.ModelOverride)
	}
	if sk.Version != "1.2.0" {
		t.Errorf("Version = %q", sk.Version)
	}
	if !strings.Contains(sk.Prompt, "senior code reviewer") {
		t.Errorf("Prompt missing body content; got %q", sk.Prompt)
	}
}

func TestParseMarkdown_NoFrontmatterDefaultsName(t *testing.T) {
	in := []byte("just a body, no frontmatter")
	sk, err := parseMarkdown(in, "filename-stem")
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "filename-stem" {
		t.Errorf("Name = %q, want filename-stem", sk.Name)
	}
	if sk.Prompt != "just a body, no frontmatter" {
		t.Errorf("Prompt = %q", sk.Prompt)
	}
}

func TestParseMarkdown_BadYAML(t *testing.T) {
	in := []byte("---\nname: : :\n  bad: indent:\n---\nbody\n")
	_, err := parseMarkdown(in, "x")
	if err == nil {
		t.Error("malformed YAML should error")
	}
	if !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error should mention yaml; got %v", err)
	}
}

func TestParseMarkdown_AutoDescription(t *testing.T) {
	// When frontmatter doesn't set description, take first non-heading
	// line of body.
	in := []byte(`---
name: x
---
# Header
First real line is the description.
Second line.
`)
	sk, _ := parseMarkdown(in, "x")
	if sk.Description != "First real line is the description." {
		t.Errorf("auto-Description = %q", sk.Description)
	}
}

func TestLoad_DispatchesByExtension(t *testing.T) {
	dir := t.TempDir()
	mdPath := filepath.Join(dir, "skill-a.md")
	jsonPath := filepath.Join(dir, "skill-b.json")
	_ = os.WriteFile(mdPath, []byte("---\nname: skill-a\ndescription: from md\n---\nbody"), 0o644)
	_ = os.WriteFile(jsonPath, []byte(`{"name":"skill-b","description":"from json"}`), 0o644)

	a, err := Load(mdPath)
	if err != nil || a.Description != "from md" {
		t.Errorf("md load failed: %v / %+v", err, a)
	}
	b, err := Load(jsonPath)
	if err != nil || b.Description != "from json" {
		t.Errorf("json load failed: %v / %+v", err, b)
	}
}

func TestLoad_UnsupportedExt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "skill.txt")
	_ = os.WriteFile(p, []byte("plain"), 0o644)
	if _, err := Load(p); err == nil {
		t.Error("unsupported ext should error")
	}
}
