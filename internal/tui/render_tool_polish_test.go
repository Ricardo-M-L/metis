package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestToolPolishBashOutputAppearsOnce(t *testing.T) {
	for _, tc := range []struct {
		name, tool, output string
		wantOccurrences    int
	}{
		{"normal", "Bash", "first evidence\nsecond evidence", 1},
		{"forwarded", "sub: Bash", "first evidence\nsecond evidence", 1},
		{"real duplicate output", "Bash", "first evidence\nfirst evidence\nsecond evidence", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := ToolEvent{
				Kind: "result", ToolName: tc.tool,
				Input:    map[string]any{"command": "python3 inspect.py"},
				Output:   tc.output,
				Duration: 388 * time.Millisecond,
			}
			if tc.tool == "sub: Bash" {
				te.SubAgentParentID = "parent"
			}
			for _, expanded := range []bool{false, true} {
				out := stripANSI(renderToolEventAtWidth(te, expanded, 100))
				if got := strings.Count(out, "first evidence"); got != tc.wantOccurrences {
					t.Fatalf("expanded=%t: output displayed %d times, want %d:\n%s", expanded, got, tc.wantOccurrences, out)
				}
				if !strings.Contains(out, "second evidence") || !strings.Contains(out, "388ms") {
					t.Fatalf("expanded=%t: output or elapsed time disappeared:\n%s", expanded, out)
				}
				if strings.Contains(out, "✓") || !strings.Contains(out, glyphBullet) {
					t.Fatalf("success should retain its leader without a duplicate checkmark:\n%s", out)
				}
			}
			// Transcript export still needs its concise standalone summary.
			if !strings.Contains(summarizeToolResult(te), "first evidence") || te.Output != tc.output {
				t.Fatal("visual deduplication changed the exported summary or raw output")
			}
		})
	}
}

func TestToolPolishSingleDetailEntryPreservesAllEvidence(t *testing.T) {
	const command = "python3 - <<'PY'\nfrom pathlib import Path\nprint(Path.cwd())\nPY"
	const evidence = "evidence 1\nevidence 2\nevidence 3\nevidence 4\nevidence 5\nevidence 6\nevidence 7"
	for _, tc := range []struct {
		name, prefix, status string
		isError              bool
	}{
		{name: "success"},
		{name: "error", isError: true, status: "✗"},
		{name: "denied", prefix: "denied: ", status: "Denied", isError: true},
		{name: "blocked", prefix: "[blocked] ", status: "Blocked", isError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := tc.prefix + evidence
			te := ToolEvent{
				ID: "fixture-command", Kind: "end", ToolName: "Bash",
				Input:  map[string]any{"command": command, "description": "Inspect fixture records"},
				Output: original, IsError: tc.isError,
			}
			compact := stripANSI(renderToolEventAtWidth(te, false, 100))
			if got := strings.Count(compact, "(ctrl+O to "); got != 1 {
				t.Fatalf("hidden command and output need one reachable detail entry, got %d:\n%s", got, compact)
			}
			for _, want := range []string{"command details", "+2 output lines", "evidence 5", tc.status} {
				if !strings.Contains(compact, want) {
					t.Errorf("collapsed output lost %q:\n%s", want, compact)
				}
			}
			if strings.Contains(compact, "evidence 6") {
				t.Fatalf("collapsed output unexpectedly displays hidden rows:\n%s", compact)
			}
			expanded := stripANSI(renderToolEventAtWidth(te, true, 100))
			for _, line := range strings.Split(command+"\n"+evidence, "\n") {
				if !strings.Contains(expanded, "       "+line+"\n") {
					t.Errorf("expanded view lost original line %q:\n%s", line, expanded)
				}
			}
			if strings.Contains(expanded, "(ctrl+O to ") || !strings.Contains(expanded, tc.status) {
				t.Errorf("expanded view still advertises hidden detail or lost result status:\n%s", expanded)
			}
			if te.Input["command"] != command || te.Output != original || te.IsError != tc.isError {
				t.Fatal("presentation changed executable input, raw evidence, or error state")
			}
		})
	}
}

func TestToolPolishLegacyQuietCommandHasNoSpuriousDetails(t *testing.T) {
	te := ToolEvent{ID: "legacy-quiet", Kind: "end", ToolName: "Bash",
		Input: map[string]any{"command": "true"}, Duration: 65 * time.Millisecond}
	out := stripANSI(renderToolEventAtWidth(te, false, 80))
	for _, want := range []string{"bash(true)", "65ms", "No output"} {
		if !strings.Contains(out, want) {
			t.Errorf("legacy quiet command lost %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ctrl+O") || strings.Contains(out, "✓") {
		t.Fatalf("quiet command has no hidden command or output to advertise:\n%s", out)
	}
	m := &Model{width: 82, toolEvents: []ToolEvent{te}}
	if ids := m.expandableToolEventIDs(); len(ids) != 0 {
		t.Fatalf("quiet command became a spurious expansion target: %v", ids)
	}
}

func TestToolPolishRetainsUsefulSummariesAndTimeoutEvidence(t *testing.T) {
	read := ToolEvent{Kind: "result", ToolName: "Read", Input: map[string]any{"path": "/tmp/fixture.go"},
		Output: "package fixture\nvar Value = 1", Duration: 12 * time.Millisecond}
	out := stripANSI(renderToolEventAtWidth(read, false, 100))
	if !strings.Contains(out, summarizeToolResult(read)) || !strings.Contains(out, "ctrl+O to expand") {
		t.Fatalf("Read must retain its useful file/line summary and expansion:\n%s", out)
	}

	timedOut := ToolEvent{Kind: "result", ToolName: "Bash", Input: map[string]any{"command": "make all"},
		Output: "compiled 12 targets\ncache saved\n[command exceeded timeout 20s]", IsError: true}
	for _, expanded := range []bool{false, true} {
		out = stripANSI(renderToolEventAtWidth(timedOut, expanded, 100))
		for _, want := range []string{"timed out after 20s", "compiled 12 targets", "cache saved", "✗"} {
			if !strings.Contains(out, want) {
				t.Errorf("expanded=%t: timeout lost %q:\n%s", expanded, want, out)
			}
		}
		if strings.Contains(out, "command exceeded timeout") || strings.Contains(out, "0ms") {
			t.Errorf("expanded=%t: raw timeout marker or synthetic duration leaked:\n%s", expanded, out)
		}
	}
}

func TestToolPolishCombinedDetailsRemainReachableByCtrlO(t *testing.T) {
	m := newSlashTestModel(t)
	m.messages = nil
	m.toolEvents = []ToolEvent{{
		ID: "combined-details", Kind: "end", ToolName: "Bash",
		Input:  map[string]any{"command": "python3 inspect.py", "description": "Inspect fixture records"},
		Output: "one\ntwo\nthree\nfour\nfive\nlast hidden evidence",
	}}
	m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if m.expandedToolID != "combined-details" {
		t.Fatalf("Ctrl+O could not reach consolidated details: %q", m.expandedToolID)
	}
	var expanded strings.Builder
	for _, item := range m.buildChatItems() {
		expanded.WriteString(stripANSI(item.Render(100)))
	}
	for _, want := range []string{"python3 inspect.py", "last hidden evidence"} {
		if !strings.Contains(expanded.String(), want) {
			t.Errorf("Ctrl+O selected tool but did not expose %q:\n%s", want, expanded.String())
		}
	}
	m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if m.expandedToolID != "" {
		t.Fatalf("single eligible call did not return to collapsed state: %q", m.expandedToolID)
	}
}

func TestToolPolishCompactModeRetainsCommandAndStandaloneSummary(t *testing.T) {
	for _, tc := range []struct {
		name, firstLine string
		isError         bool
	}{
		{name: "success", firstLine: "Found 9 historical records"},
		{name: "error", firstLine: "fatal: database unavailable", isError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := ToolEvent{
				Kind: "end", ToolName: "Bash", Duration: 388 * time.Millisecond,
				Input:   map[string]any{"command": "python3 inspect.py", "description": "Inspect fixture records"},
				Output:  tc.firstLine + "\nsecond output line\nthird\nfourth\nfifth\nsixth",
				IsError: tc.isError,
			}
			item := compactToolEventItem{te: te}
			out := strings.TrimRight(stripANSI(item.Render(100)), "\n")
			if lines := strings.Split(out, "\n"); len(lines) != 2 {
				t.Fatalf("compact result must occupy exactly two rows, got %d:\n%s", len(lines), out)
			}
			for _, want := range []string{"bash(python3 inspect.py)", "388ms", tc.firstLine} {
				if !strings.Contains(out, want) {
					t.Errorf("compact output lost %q:\n%s", want, out)
				}
			}
			for _, unwanted := range []string{"Inspect fixture records", "second output line", "sixth", "ctrl+O", "command details"} {
				if strings.Contains(out, unwanted) {
					t.Errorf("compact output exposed a body or unreachable detail %q:\n%s", unwanted, out)
				}
			}
			if tc.isError && !strings.Contains(out, "✗") {
				t.Errorf("compact error lost its failure indicator:\n%s", out)
			}
		})
	}
}
