package tui

// Phase-D #41 — only the pure helpers are unit-test friendly.
// tea.ExecProcess pauses bubbletea, which we don't bring up in unit
// tests. Coverage:
//   - writeDraftToTemp roundtrips body content
//   - applyExternalEditorResult trims trailing newline + sets value

import (
	"os"
	"strings"
	"testing"

	textarea "charm.land/bubbles/v2/textarea"
)

func TestWriteDraftToTemp_RoundTrip(t *testing.T) {
	body := "line one\nline two\n"
	path, err := writeDraftToTemp(body)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	defer os.Remove(path)

	if !strings.HasSuffix(path, ".md") {
		t.Errorf(".md extension expected for syntax highlight; got %q", path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Errorf("body roundtrip mismatch: got %q want %q", got, body)
	}
}

func TestApplyExternalEditorResult_TrimsTrailingNewline(t *testing.T) {
	m := &Model{input: textarea.New()}
	m.input.SetValue("old text")
	msg := externalEditorDoneMsg{contents: "new\nmulti\nline\n"}
	m.applyExternalEditorResult(msg)
	if got := m.input.Value(); got != "new\nmulti\nline" {
		t.Errorf("trailing newline should be trimmed; got %q", got)
	}
}

func TestApplyExternalEditorResult_ErrorSurfaced(t *testing.T) {
	m := &Model{input: textarea.New()}
	msg := externalEditorDoneMsg{err: os.ErrNotExist}
	m.applyExternalEditorResult(msg)
	// Last message should be an error row.
	if len(m.messages) == 0 {
		t.Fatalf("expected an error message row")
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != "error" {
		t.Errorf("expected role=error; got %q", last.Role)
	}
}

func TestConfigSlashReturnsManagedExecCommand(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := newSlashTestModel(t)
	m.input.SetValue("/config")
	cmd := pressEnter(t, m)
	if cmd == nil {
		t.Fatal("/config must return a Bubble Tea managed editor command")
	}
	for _, msg := range m.messages {
		if strings.Contains(msg.Content, "config updated") || strings.Contains(msg.Content, "config saved") {
			t.Fatalf("/config reported success before editor exited: %+v", msg)
		}
	}
}
