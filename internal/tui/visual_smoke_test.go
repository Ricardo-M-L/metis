package tui

import (
	"strings"
	"testing"
)

// visual_smoke_test.go — direct View() inspection tests for the
// 2026-05-06 round of features (F10 / F11 / F12 / F22 / F23 / F25).
// These complement the PTY/expect tests which can't capture bubbletea
// view content; here we drive Model.View() and grep the rendered
// string for the markers each feature should emit.

// TestF12_StatusBar_RendersBranchAndSession — a populated Model with
// non-empty sessionID should surface "❉ <6-char>" in the status bar
// row. (Branch glyph "⎇" is omitted in the test because it depends
// on cwd being a git repo, which test temp dirs aren't.)
func TestF12_StatusBar_RendersSessionGlyph(t *testing.T) {
	m := newE2EModel(t, 120, 30, 3)
	m.sessionID = "abcdef0123456789"
	view := m.View().Content
	if !strings.Contains(view, "❉ abcdef") {
		t.Errorf("status bar should include session-id glyph + 6-char prefix; got fragment:\n%s",
			extractContaining(view, "❉", "tokens"))
	}
}

// TestF10_TranscriptSearch_OverlayRenders — flipping showSearch =
// true with a query should produce the "search transcript" overlay
// box.
func TestF10_TranscriptSearch_OverlayRenders(t *testing.T) {
	m := newE2EModel(t, 120, 30, 5)
	m.showSearch = true
	m.searchQuery = "test"
	m.recomputeSearchHits()
	view := m.View().Content
	if !strings.Contains(view, "search transcript") {
		t.Errorf("search overlay header missing; view:\n%s", excerpt(view, 200))
	}
	if !strings.Contains(view, "/test/") {
		t.Errorf("search query body missing /test/; view:\n%s", excerpt(view, 200))
	}
}

// TestF25_Cursor_AttachedWhenInputFocused — Model.View() should set
// tea.View.Cursor to a non-nil pointer when the input is focused
// and no overlay is consuming the keyboard. This is what makes the
// terminal show a native block cursor (F25).
//
// newE2EModel uses textarea.New() defaults (virtualCursor=true,
// not Focused()) — those bypass the NewModel init path that makes
// Cursor() return non-nil. We replicate the relevant init bits
// here so the test exercises the cursor-attach path in isolation.
func TestF25_Cursor_AttachedWhenInputFocused(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.input.SetVirtualCursor(false)
	m.input.Focus()
	v := m.View()
	if v.Cursor == nil {
		t.Fatal("V.Cursor must be non-nil so the terminal renders a cursor")
	}
	if v.Cursor.Position.Y < 1 {
		t.Errorf("cursor Y should be > 0 (somewhere in the textarea body); got %d", v.Cursor.Position.Y)
	}
}

// TestF25_Cursor_NilDuringActiveScreen — when an overlay screen is
// active the cursor should be nil; the overlay owns the keyboard
// and a stray block cursor on top of it confuses the user.
func TestF25_Cursor_NilDuringSearchOverlay(t *testing.T) {
	m := newE2EModel(t, 120, 30, 3)
	m.showSearch = true
	v := m.View()
	if v.Cursor != nil {
		t.Errorf("cursor must be nil while showSearch — overlay owns keyboard; got %+v", v.Cursor)
	}
}

// TestF11_FullTranscriptYank_BuildsPayload — yankFullTranscript on a
// populated model should write something to the clipboard fallback
// file and return a non-empty status. We don't assert the OSC 52
// bytes (writeClipboard fires those at stdout where tests can't
// observe), but the fallback file path is reproducible.
func TestF11_FullTranscriptYank_BuildsPayload(t *testing.T) {
	m := newE2EModel(t, 120, 30, 5)
	status := m.yankFullTranscript()
	if !strings.Contains(status, "copied transcript") {
		t.Errorf("expected copied-transcript status; got %q", status)
	}
	if !strings.Contains(status, "turn") {
		t.Errorf("status should mention turn count; got %q", status)
	}
}

// TestF23_UpdateNotice_StashedForNextNewModel — newE2EModel skips
// NewModel's init path so the notice consumption can't be tested
// end-to-end from the fixture. Instead we verify the package-level
// setter correctly stashes the notice for the next NewModel call;
// the consumption is tested manually in metis chat (smoke).
func TestF23_UpdateNotice_StashedForNextNewModel(t *testing.T) {
	SetPendingUpdateNotice("metis 9.9.9 available — run `metis update`")
	defer SetPendingUpdateNotice("")
	if pendingUpdateNotice != "metis 9.9.9 available — run `metis update`" {
		t.Errorf("SetPendingUpdateNotice should stash the notice; got %q", pendingUpdateNotice)
	}
}

// extractContaining returns the lines around the first occurrence of
// `marker` in s. Used to keep test failure output focused.
func extractContaining(s string, markers ...string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		for _, m := range markers {
			if strings.Contains(line, m) {
				start := i - 1
				if start < 0 {
					start = 0
				}
				end := i + 2
				if end > len(lines) {
					end = len(lines)
				}
				return strings.Join(lines[start:end], "\n")
			}
		}
	}
	return excerpt(s, 200)
}

// excerpt returns the first n chars of s, with an ellipsis when
// truncation occurred.
func excerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
