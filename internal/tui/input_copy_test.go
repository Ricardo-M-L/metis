package tui

// input_copy_test.go — coverage for the "let me copy whatever's in the
// input box" affordances added 2026-05-16 in response to user
// screenshot 34 ("蓝色框起来的输入框的内容不能复制, 鼠标选中文字
// 没显示选中的阴影"). Two paths:
//
//  1. Ctrl+S copy-mode transcript dump must include the current draft
//     so it lands in the user's native scrollback when alt-screen exits.
//  2. Alt+Y instantly copies m.input.Value() to the clipboard fallback.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildPlainTranscript_IncludesCurrentInput — after the user types
// something but before they submit, Ctrl+S enters copy mode and dumps
// the transcript. The draft must appear as a trailing "> ..." line so
// the user can mouse-select it natively from terminal scrollback.
func TestBuildPlainTranscript_IncludesCurrentInput(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.messages = append(m.messages, Message{Role: "assistant", Content: "hi"})
	m.input.SetValue("draft to copy")

	got := m.buildPlainTranscript()

	if !strings.Contains(got, "● hi") {
		t.Errorf("transcript missing assistant line; got %q", got)
	}
	if !strings.Contains(got, "> draft to copy") {
		t.Errorf("transcript missing current input as '> draft to copy'; got %q", got)
	}
}

// TestBuildPlainTranscript_OmitsEmptyInput — when the input is empty,
// no spurious "> " line should appear. Otherwise users in copy mode see
// a dangling prompt arrow which reads as a bug.
func TestBuildPlainTranscript_OmitsEmptyInput(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.messages = append(m.messages, Message{Role: "assistant", Content: "hello"})
	m.input.SetValue("")

	got := m.buildPlainTranscript()

	// The expected trailing line is "\n● hello\n" — no further "> "
	// after that.
	trailing := strings.TrimRight(got, "\n")
	if strings.HasSuffix(trailing, ">") || strings.Contains(trailing, "\n> \n") {
		t.Errorf("transcript should not append empty draft line; got %q", got)
	}
}

// TestAltY_CopiesInputToClipboardFile — Alt+Y triggers writeClipboard
// which (in non-OSC-52 fallback) writes ~/.metis/clipboard.txt. Verify
// the file gets the input box content and an info message lands in
// m.messages.
func TestAltY_CopiesInputToClipboardFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("METIS_HOME", tmp)
	t.Setenv("HOME", tmp)

	m := newE2EModel(t, 120, 30, 0)
	m.input.SetValue("path/to/copy")

	before := len(m.messages)
	pressKey(t, m, "alt+y")

	body, err := os.ReadFile(filepath.Join(tmp, "clipboard.txt"))
	if err != nil {
		t.Fatalf("clipboard fallback file missing: %v", err)
	}
	if string(body) != "path/to/copy" {
		t.Errorf("clipboard content: want %q, got %q", "path/to/copy", body)
	}
	if len(m.messages) != before+1 {
		t.Errorf("alt+y should append one info row; got %d → %d", before, len(m.messages))
	}
	if msg := m.messages[len(m.messages)-1]; msg.Role != "info" ||
		!strings.Contains(msg.Content, "copied") {
		t.Errorf("info row should say 'copied'; got role=%q content=%q",
			msg.Role, msg.Content)
	}
}

// TestAltY_EmptyInputReportsNothingToCopy — pressing Alt+Y on an empty
// input must not write garbage to the clipboard and must surface a
// friendly status line.
func TestAltY_EmptyInputReportsNothingToCopy(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("METIS_HOME", tmp)
	t.Setenv("HOME", tmp)

	m := newE2EModel(t, 120, 30, 0)
	m.input.SetValue("")

	before := len(m.messages)
	pressKey(t, m, "alt+y")

	// Clipboard fallback file should NOT exist (or, if some other code
	// path created it, should not contain user input).
	if _, err := os.Stat(filepath.Join(tmp, "clipboard.txt")); err == nil {
		t.Errorf("alt+y on empty input should not touch clipboard file")
	}
	if len(m.messages) != before+1 {
		t.Errorf("alt+y empty should still append one info row; got %d → %d", before, len(m.messages))
	}
	if msg := m.messages[len(m.messages)-1]; !strings.Contains(msg.Content, "empty") {
		t.Errorf("info row should say 'empty'; got %q", msg.Content)
	}
}
