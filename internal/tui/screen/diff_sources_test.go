package screen

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func sourceFile(path, line string) DiffFile {
	return DiffFile{
		Path: path, Status: "M",
		Hunks: []DiffHunk{{
			OldStart: 1, OldCount: 1, NewStart: 1, NewCount: 1,
			Lines: []DiffLine{{Type: "+", Content: line, NewNum: 1}},
		}},
	}
}

func TestDiffViewerSwitchesSourcesWithoutMutatingThem(t *testing.T) {
	sources := []DiffSource{
		{Label: "Working tree", Files: []DiffFile{sourceFile("work.go", "work")}},
		{Label: "Turn 2", Files: []DiffFile{sourceFile("turn.go", "turn")}},
	}
	s := NewDiffViewerScreenWithSources(sources)
	s.Resize(100, 30)
	if view := s.View(); !strings.Contains(view, "Working tree") || !strings.Contains(view, "work.go") {
		t.Fatalf("initial source missing:\n%s", view)
	}

	_, _ = s.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if view := s.View(); !strings.Contains(view, "Turn 2") || !strings.Contains(view, "turn.go") || strings.Contains(view, "work.go") {
		t.Fatalf("right did not select turn source:\n%s", view)
	}
	if got := sources[0].Files[0].Path; got != "work.go" {
		t.Fatalf("source input mutated: %q", got)
	}

	_, _ = s.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !s.Done() {
		t.Fatal("Esc from source file list should close viewer")
	}
	if sources[1].Files[0].Path != "turn.go" {
		t.Fatal("closing viewer mutated turn source")
	}
}

func TestDiffViewerExplainsCleanWorkingTree(t *testing.T) {
	s := NewDiffViewerScreenWithSources([]DiffSource{{Label: "Working tree"}})
	s.Resize(100, 30)
	view := s.View()
	if !strings.Contains(view, "Working tree clean") || !strings.Contains(view, "0 files") {
		t.Fatalf("clean state is unclear:\n%s", view)
	}
}

func TestDiffViewerShowsCollectionErrorInsteadOfCleanState(t *testing.T) {
	s := NewDiffViewerScreenWithSources([]DiffSource{{
		Label: "Working tree",
		Error: "not a git repository",
	}})
	s.Resize(100, 30)
	view := s.View()
	if !strings.Contains(view, "Unable to load working tree changes") ||
		!strings.Contains(view, "not a git repository") ||
		strings.Contains(view, "Working tree clean") {
		t.Fatalf("collector error was presented as a clean tree:\n%s", view)
	}
}

func TestDiffViewerNeutralizesUntrustedPathsAndHunkContents(t *testing.T) {
	rawPath := "folder/evil\x1b]52;c;Y29weQ==\x07\ninjected.go" + strings.Repeat("模", 160)
	rawContent := "payload\x1b[2J\x1b]52;c;c2VjcmV0\x07\tend"
	sources := []DiffSource{{
		Label: "Working tree",
		Files: []DiffFile{{
			Path: rawPath, Status: "M",
			Hunks: []DiffHunk{{
				OldStart: 1, OldCount: 1, NewStart: 1, NewCount: 1,
				Lines: []DiffLine{{Type: "+", Content: rawContent, NewNum: 1}},
			}},
		}},
	}}
	s := NewDiffViewerScreenWithSources(sources)
	s.Resize(80, 24)
	listView := s.View()
	for _, unsafe := range []string{"\x1b]52", "\x1b[2J", "\x07", "evil\x1b", "\ninjected.go"} {
		if strings.Contains(listView, unsafe) {
			t.Fatalf("unsafe path control %q reached terminal output:\n%q", unsafe, listView)
		}
	}
	if !strings.Contains(listView, `\x1b]52`) || !strings.Contains(listView, "⏎") || !strings.Contains(listView, "…") {
		t.Fatalf("path controls were not rendered visibly and bounded:\n%s", listView)
	}

	_, _ = s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	diffView := s.View()
	for _, unsafe := range []string{"\x1b]52", "\x1b[2J", "\x07", "\t"} {
		if strings.Contains(diffView, unsafe) {
			t.Fatalf("unsafe hunk control %q reached terminal output:\n%q", unsafe, diffView)
		}
	}
	if !strings.Contains(diffView, `\x1b[2J`) || !strings.Contains(diffView, `\x1b]52`) || !strings.Contains(diffView, "⇥") {
		t.Fatalf("hunk controls were not neutralized visibly:\n%s", diffView)
	}
	if sources[0].Files[0].Path != rawPath || sources[0].Files[0].Hunks[0].Lines[0].Content != rawContent {
		t.Fatal("render boundary rewrote the underlying patch structure")
	}
}

func TestDiffViewerNeutralizesSourceMetadata(t *testing.T) {
	s := NewDiffViewerScreenWithSources([]DiffSource{{
		Label:    "work\x1b]52;c;bad\x07",
		Subtitle: "prompt\x1b[2J",
		Error:    "failure\nspoof",
	}})
	s.Resize(80, 24)
	view := s.View()
	for _, unsafe := range []string{"\x1b]52", "\x1b[2J", "\x07", "failure\nspoof"} {
		if strings.Contains(view, unsafe) {
			t.Fatalf("unsafe source metadata %q reached terminal output:\n%q", unsafe, view)
		}
	}
	for _, visible := range []string{`\x1b]52`, `\x1b[2J`, "⏎"} {
		if !strings.Contains(view, visible) {
			t.Fatalf("neutralized source metadata %q missing:\n%s", visible, view)
		}
	}
}

func TestSafeDiffDisplayTextBoundsUnicodeByTerminalWidth(t *testing.T) {
	got := safeDiffDisplayText(strings.Repeat("模", 20), 11)
	if width := lipgloss.Width(got); width > 11 {
		t.Fatalf("display width = %d, want <= 11: %q", width, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated label lacks ellipsis: %q", got)
	}
}
