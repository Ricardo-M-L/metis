package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
)

func TestBashDescriptionDistinguishesCommandsWithoutHidingDetails(t *testing.T) {
	for _, kind := range []string{"result", "end"} {
		for _, tool := range []string{"Bash", "sub: Bash"} {
			for i, description := range []string{"查看历史会话中的改名讨论", "检查仓库中的改名提交"} {
				t.Run(fmt.Sprintf("%s/%s/%d", kind, tool, i), func(t *testing.T) {
					command := fmt.Sprintf("python3 - <<'PY'\nprint('search-%d')\nPY", i)
					input := map[string]any{"command": command, "description": description}
					original := map[string]any{"command": command, "description": description}
					te := ToolEvent{ID: "described-shell", Kind: kind, ToolName: tool, Input: input}
					if tool == "sub: Bash" {
						te.SubAgentParentID = "parent-agent"
					}
					compact := stripANSI(renderToolEventAtWidth(te, false, 80))
					header, _, _ := strings.Cut(compact, "\n")
					if !strings.Contains(header, "bash("+description+")") || strings.Contains(header, "python3") {
						t.Fatalf("header should identify the purpose of this shell call: %q", header)
					}
					m := &Model{width: 82, toolEvents: []ToolEvent{te}}
					ids := m.expandableToolEventIDs()
					if len(ids) != 1 || ids[0] != te.ID {
						t.Fatalf("original command is not reachable through Ctrl+O: %v", ids)
					}
					expanded := stripANSI(renderToolEventAtWidth(te, true, 80))
					for _, line := range strings.Split(command, "\n") {
						if !strings.Contains(expanded, "       "+line+"\n") {
							t.Errorf("expanded view lost command line %q:\n%s", line, expanded)
						}
					}
					if !reflect.DeepEqual(input, original) || toolArgsPreview(tool, input) != command {
						t.Fatal("descriptive header changed the executed input or raw command preview")
					}
				})
			}
		}
	}
}

func TestBashDescriptionFallbackAndOtherToolHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, tool, want string
		input            map[string]any
	}{
		{"legacy", "Bash", "python3 - <<'PY' …", map[string]any{"command": "python3 - <<'PY'\nprint('ok')\nPY"}},
		{"empty", "Bash", "git status", map[string]any{"command": "git status", "description": " \t\r\n"}},
		{"control only", "Bash", "git status", map[string]any{"command": "git status", "description": "\x1b[31m\a\x1b[0m"}},
		{"invalid type", "Bash", "git status", map[string]any{"command": "git status", "description": 42}},
		{"no input", "Bash", "", nil},
		{"other tool", "Glob", "**/*.go", map[string]any{"pattern": "**/*.go", "description": "ignore this description"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := stripANSI(renderToolEventAtWidth(ToolEvent{Kind: "start", ToolName: tc.tool, Input: tc.input}, false, 100))
			if tc.want != "" && !strings.Contains(out, "("+tc.want+")") {
				t.Fatalf("header changed the legacy or non-shell preview: %q", out)
			}
			if strings.Count(out, "\n") != 1 || strings.Contains(out, "ignore this description") {
				t.Fatalf("unexpected header: %q", out)
			}
		})
	}
}

func TestBashDescriptionStaysInOneTerminalRow(t *testing.T) {
	description := "\x1b[31m查找\t历史\r\n记录\u2028并核对\a\x1b[0m"
	input := map[string]any{"command": "git log", "description": description}
	te := ToolEvent{Kind: "start", ToolName: "Bash", Input: input}
	out := stripANSI(renderToolEventAtWidth(te, false, 80))
	if !strings.Contains(out, "bash(查找 历史 记录 并核对)") || strings.Count(out, "\n") != 1 {
		t.Fatalf("description did not become one readable header: %q", out)
	}
	if input["description"] != description {
		t.Fatal("header normalization changed the stored description")
	}

	input["description"] = strings.Repeat("核对👩‍💻历史记录", 100)
	for _, width := range []int{24, 40, 80, 400} {
		out = stripANSI(renderToolEventAtWidth(te, false, width))
		if strings.Count(out, "\n") != 1 {
			t.Fatalf("long description introduced extra rows: %q", out)
		}
		if cells := xansi.StringWidth(strings.TrimSuffix(out, "\n")); cells > min(width, toolArgsPreviewMaxRunes+12) {
			t.Errorf("description exceeds viewport or preview cap at width %d: %d cells", width, cells)
		}
	}
}
