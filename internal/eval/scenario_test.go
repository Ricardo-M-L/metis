package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempScenario(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "scn.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadScenario_Full(t *testing.T) {
	body := `---
id: smoke_read
description: agent reads README and summarises
tags: smoke, basic
timeout_seconds: 30
---

# Setup
empty workspace

# Prompt
read README.md and tell me what this project is in one sentence.

# Reward
- contains_all: ["metis", "agent"] weight=2
- used_tool: Read weight=1
- length: 10..400 weight=0.5
`
	s, err := LoadScenario(writeTempScenario(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "smoke_read" {
		t.Errorf("id = %q; want smoke_read", s.ID)
	}
	if s.Timeout.Seconds() != 30 {
		t.Errorf("timeout = %v; want 30s", s.Timeout)
	}
	if len(s.Tags) != 2 {
		t.Errorf("tags = %v; want 2 entries", s.Tags)
	}
	if !strings.Contains(s.Prompt, "README.md") {
		t.Errorf("prompt missing expected text; got %q", s.Prompt)
	}
	if len(s.Assertions) != 3 {
		t.Fatalf("assertions = %d; want 3", len(s.Assertions))
	}
	a0 := s.Assertions[0]
	if a0.Type != AssertContainsAll || a0.Weight != 2 {
		t.Errorf("first assertion wrong: %+v", a0)
	}
	if a0.Tokens[0] != "metis" {
		t.Errorf("token = %q; want metis", a0.Tokens[0])
	}
	a1 := s.Assertions[1]
	if a1.Type != AssertUsedTool || a1.Tool != "Read" {
		t.Errorf("used_tool wrong: %+v", a1)
	}
	a2 := s.Assertions[2]
	if a2.Type != AssertLength || a2.LengthMin != 10 || a2.LengthMax != 400 {
		t.Errorf("length wrong: %+v", a2)
	}
}

func TestLoadScenario_NoFrontmatter(t *testing.T) {
	body := `# Prompt
hello

# Reward
- contains_all: ["hi"]
`
	path := writeTempScenario(t, body)
	s, err := LoadScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "scn" { // derived from filename "scn.md"
		t.Errorf("id should default to filename stem; got %q", s.ID)
	}
	if s.Timeout.Seconds() != 60 { // default
		t.Errorf("default timeout should be 60s; got %v", s.Timeout)
	}
}

func TestLoadScenario_MissingPrompt(t *testing.T) {
	body := `---
id: x
---

# Reward
- contains_all: ["y"]
`
	if _, err := LoadScenario(writeTempScenario(t, body)); err == nil {
		t.Error("missing # Prompt should error")
	}
}

func TestLoadScenario_MissingReward(t *testing.T) {
	body := `---
id: x
---

# Prompt
hi
`
	if _, err := LoadScenario(writeTempScenario(t, body)); err == nil {
		t.Error("missing # Reward should error")
	}
}

func TestLoadScenarios_SkipsReadme(t *testing.T) {
	dir := t.TempDir()
	good := `---
id: ok
---

# Prompt
go
# Reward
- contains_all: [foo]
`
	if err := os.WriteFile(filepath.Join(dir, "good.md"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# explainer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scenarios, err := LoadScenarios(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 1 || scenarios[0].ID != "ok" {
		t.Errorf("expected only the good scenario; got %v", scenarios)
	}
}

func TestParseAssertions_AllTypes(t *testing.T) {
	body := `
- contains_all: ["a", "b"]
- contains_any: [c, d, e] weight=0.5
- not_contains: [bad]
- used_tool: Glob
- regex: ^hello weight=2
- length: 5..100
`
	got, err := parseAssertions(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("want 6 assertions; got %d", len(got))
	}
	types := make([]AssertionType, len(got))
	for i, a := range got {
		types[i] = a.Type
	}
	wantTypes := []AssertionType{
		AssertContainsAll, AssertContainsAny, AssertNotContains,
		AssertUsedTool, AssertRegex, AssertLength,
	}
	for i := range wantTypes {
		if types[i] != wantTypes[i] {
			t.Errorf("[%d] = %s; want %s", i, types[i], wantTypes[i])
		}
	}
	if got[1].Weight != 0.5 {
		t.Errorf("contains_any weight should be 0.5; got %v", got[1].Weight)
	}
	if got[4].Weight != 2 {
		t.Errorf("regex weight should be 2; got %v", got[4].Weight)
	}
}

func TestParseAssertions_UnknownKeyIgnored(t *testing.T) {
	body := `
- future_assertion_type: anything weight=99
- contains_all: [hi]
`
	got, err := parseAssertions(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("unknown key should be skipped silently; got %d assertions", len(got))
	}
}

func TestSplitList_Variants(t *testing.T) {
	cases := map[string][]string{
		`["a", "b", "c"]`: {"a", "b", "c"},
		`a, b, c`:         {"a", "b", "c"},
		`['x']`:           {"x"},
		``:                nil,
		`[]`:              nil,
	}
	for in, want := range cases {
		got := splitList(in)
		if len(got) != len(want) {
			t.Errorf("splitList(%q): got %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("splitList(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}

func TestParseRange(t *testing.T) {
	lo, hi, ok := parseRange("10..200")
	if !ok || lo != 10 || hi != 200 {
		t.Errorf("parseRange('10..200') = (%d,%d,%v); want (10,200,true)", lo, hi, ok)
	}
	if _, _, ok := parseRange("not a range"); ok {
		t.Error("garbage should not parse")
	}
}
