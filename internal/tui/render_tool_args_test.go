package tui

import (
	"fmt"
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
)

func TestToolLeaderPreviewKeepsMultilineInputInOneRow(t *testing.T) {
	for _, tc := range []struct {
		name, tool, key, input, want string
	}{
		{"heredoc", "Bash", "command", "swift - <<'SWIFT'\nimport Foundation\nimport AppKit\nSWIFT", "swift - <<'SWIFT' …"},
		{"crlf", "Bash", "command", "printf first\r\nprintf second", "printf first …"},
		{"subagent", "sub: Bash", "command", "printf first\nprintf second", "printf first …"},
		{"query", "WebSearch", "query", "terminal layout\nline wrapping", "terminal layout …"},
		{"unicode separator", "Bash", "command", "printf first\u2028printf second", "printf first …"},
		{"terminal controls", "Bash", "command", "\x1b[31mprintf\x1b[0m\t你好\a", "printf 你好"},
		{"quoted spacing", "Bash", "command", `printf 'two  spaces'`, `printf 'two  spaces'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := map[string]any{tc.key: tc.input}
			te := ToolEvent{Kind: "start", ToolName: tc.tool, Input: input}
			out := stripANSI(renderToolEvent(te, false))
			if !strings.Contains(out, "("+tc.want+")") || strings.Count(out, "\n") != 1 {
				t.Errorf("compact leader = %q, want one row containing %q", out, tc.want)
			}
			if input[tc.key] != tc.input {
				t.Fatal("rendering changed the original tool input")
			}
		})
	}
}

func TestToolLeaderHeredocStaysInsideViewport(t *testing.T) {
	command := "swift - /项目/截图.png <<'SWIFT'\nimport Foundation\nimport AppKit\nprint(\"你好\")\nSWIFT"
	for _, width := range []int{24, 40, 80, 120} {
		for _, subagent := range []bool{false, true} {
			t.Run(fmt.Sprintf("width=%d/subagent=%t", width, subagent), func(t *testing.T) {
				te := ToolEvent{Kind: "start", ToolName: "Bash", Input: map[string]any{"command": command}}
				if subagent {
					te.ToolName, te.SubAgentParentID = "sub: Bash", "agent-1"
				}
				out := stripANSI(renderToolEventAtWidth(te, false, width))
				if strings.Count(out, "\n") != 1 {
					t.Fatalf("tool leader must occupy one row, got %q", out)
				}
				if strings.Contains(out, "import") {
					t.Fatalf("heredoc body leaked into the compact leader: %q", out)
				}
				if got := xansi.StringWidth(strings.TrimSuffix(out, "\n")); got > width {
					t.Fatalf("leader width %d exceeds viewport %d: %q", got, width, out)
				}
			})
		}
	}
}

func TestExpandedBashRetainsFullCommandWithIndentedWrapping(t *testing.T) {
	for _, command := range []string{
		"swift - <<'SWIFT'\nimport Foundation\nimport AppKit\nprint(\"你好\")\nSWIFT",
		"printf '" + strings.Repeat("你好", 160) + "'",
	} {
		for _, width := range []int{40, 80} {
			t.Run(fmt.Sprintf("width=%d/length=%d", width, len(command)), func(t *testing.T) {
				te := ToolEvent{Kind: "result", ToolName: "Bash", Input: map[string]any{"command": command}, Output: "OK"}
				out := stripANSI(renderToolEventAtWidth(te, true, width))
				lines := strings.Split(out, "\n")
				var body strings.Builder
				for _, line := range lines[1:] {
					if xansi.StringWidth(line) > width {
						t.Fatalf("expanded row exceeds viewport %d: %q", width, line)
					}
					if strings.HasPrefix(line, "       ") {
						body.WriteString(strings.TrimPrefix(line, "       "))
					}
				}
				if !strings.Contains(body.String(), strings.ReplaceAll(command, "\n", "")) {
					t.Fatalf("expanded command was lost, truncated, or not indented:\n%s", out)
				}
				if te.Input["command"] != command {
					t.Fatal("expanded rendering changed the executed command")
				}
			})
		}
	}
}

func TestHiddenBashCommandIsReachableByExpandShortcut(t *testing.T) {
	for _, tc := range []struct {
		name, kind, command string
	}{
		{"resumed heredoc", "end", "python3 - <<'PY'\nimport json\nPY"},
		{"viewport truncation", "result", "printf '" + strings.Repeat("wide", 20) + "'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Model{
				width: 40,
				toolEvents: []ToolEvent{{ID: "command-1", Kind: tc.kind, ToolName: "Bash",
					Input: map[string]any{"command": tc.command}, Output: "OK"}},
			}
			ids := m.expandableToolEventIDs()
			if len(ids) != 1 || ids[0] != "command-1" {
				t.Fatalf("Ctrl+O cannot reach the hidden command: %v", ids)
			}
		})
	}
}

func TestHiddenBashCommandRemainsExpandableForCompactResults(t *testing.T) {
	for _, result := range []struct {
		name, output, label string
	}{
		{"no matches", "exit status 1", "No matches"},
		{"denied", "denied: user rejected", "Denied"},
		{"blocked", "[blocked] policy rejected", "Blocked"},
	} {
		for _, kind := range []string{"result", "end"} {
			for _, width := range []int{40, 80} {
				for _, multiline := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/width=%d/multiline=%t", result.name, kind, width, multiline), func(t *testing.T) {
						command := "rg first"
						if multiline {
							command += "\nrg second"
						}
						te := ToolEvent{ID: "command-1", Kind: kind, ToolName: "Bash",
							Input: map[string]any{"command": command}, Output: result.output, IsError: true}
						m := &Model{width: width, toolEvents: []ToolEvent{te}}
						compact := stripANSI(renderToolEventAtWidth(te, false, width-2))
						if !strings.Contains(compact, result.label) {
							t.Fatalf("compact result lost its status %q:\n%s", result.label, compact)
						}
						if got := strings.Contains(compact, "(ctrl+O to "); got != multiline {
							t.Errorf("expand hint = %t, want %t:\n%s", got, multiline, compact)
						}
						ids := m.expandableToolEventIDs()
						if multiline {
							if len(ids) != 1 || ids[0] != te.ID {
								t.Errorf("Ctrl+O cannot reach the hidden command: %v", ids)
							}
							expanded := stripANSI(renderToolEventAtWidth(te, true, width-2))
							if !strings.Contains(expanded, "       rg second\n") || !strings.Contains(expanded, result.label) {
								t.Errorf("expansion lost command detail or changed result status:\n%s", expanded)
							}
							if strings.Contains(expanded, "(ctrl+O to ") {
								t.Errorf("expanded command still advertises hidden detail:\n%s", expanded)
							}
						} else if len(ids) != 0 {
							t.Errorf("short single-line result unexpectedly became expandable: %v", ids)
						}
						if te.Input["command"] != command || te.Output != result.output || !te.IsError {
							t.Fatal("display changed the tool input, output, or error state")
						}
					})
				}
			}
		}
	}
}

func TestToolLeaderTruncationPreservesGraphemeTail(t *testing.T) {
	got := truncateMiddleCells("aaaaaaaaaa👩‍💻", 8)
	if got != "aa … a👩‍💻" {
		t.Fatalf("middle truncation split a terminal grapheme: %q", got)
	}
}

func TestExpandedRecoveredBashCommandUsesViewportWidth(t *testing.T) {
	command := "printf '" + strings.Repeat("alignment", 12) + "'\nprintf 'second'"
	group := &recoveredErrorGroupItem{
		events: []ToolEvent{{Kind: "result", ToolName: "Bash",
			Input: map[string]any{"command": command}, Output: "failure", IsError: true}},
		expand: true,
	}
	out := stripANSI(group.Render(40))
	var body strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if cells := xansi.StringWidth(line); cells > 40 {
			t.Fatalf("expanded recovered row exceeds viewport: %q (%d cells)", line, cells)
		}
		if strings.HasPrefix(line, "       ") {
			body.WriteString(strings.TrimPrefix(line, "       "))
		}
	}
	if !strings.Contains(body.String(), strings.ReplaceAll(command, "\n", "")) {
		t.Fatalf("expanded recovered command was lost or not indented:\n%s", out)
	}
}

func TestToolLeaderTruncationFitsWideGraphemeBoundary(t *testing.T) {
	if got := truncateMiddleCells("aaaaaaaaaa界終", 8); got != "aa … 終" {
		t.Fatalf("wide suffix must fit its cell budget and retain its end: %q", got)
	}
	for _, value := range []string{"aaaaaaaaaa界終", "prefix-👩‍💻界終", strings.Repeat("目录", 20) + "/文件.go"} {
		for width := 1; width <= 40; width++ {
			got := truncateMiddleCells(value, width)
			if cells := xansi.StringWidth(got); cells > width {
				t.Errorf("truncateMiddleCells(%q, %d) = %q (%d cells)", value, width, got, cells)
			}
		}
	}
}
