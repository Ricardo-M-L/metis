package screen

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestDiffViewerFileCursorStaysVisible(t *testing.T) {
	files := make([]DiffFile, 30)
	for i := range files {
		files[i].Path = fmt.Sprintf("file-%02d", i)
	}
	s := NewDiffViewerScreen(files)
	s.Resize(80, 12) // seven visible file rows
	for range 12 {
		s.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if s.scroll == 0 || s.cursor < s.scroll || s.cursor >= s.scroll+s.bodyHeight() {
		t.Fatalf("cursor=%d scroll=%d body=%d; cursor should remain visible", s.cursor, s.scroll, s.bodyHeight())
	}
}

func TestResumeSearchAcceptsUnicodeRune(t *testing.T) {
	key := tea.KeyPressMsg{Code: '中', Text: "中"}
	backspace := tea.KeyPressMsg{Code: tea.KeyBackspace}

	resume := NewResumeScreen([]SessionEntry{{ID: "1", Title: "中文"}})
	resume.searching = true
	resume.Update(key)
	if resume.filter != "中" || len(resume.filtered) != 1 {
		t.Fatalf("resume filter=%q matches=%d", resume.filter, len(resume.filtered))
	}
	resume.Update(backspace)
	if resume.filter != "" {
		t.Fatalf("resume backspace left invalid filter %q", resume.filter)
	}
}
