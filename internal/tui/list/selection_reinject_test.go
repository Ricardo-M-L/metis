package list

// selection_reinject_test.go — locks the fix for the fragmented
// selection bg bug (image #2 user report 2026-05-10).
//
// Before: a styled chat line like
//   "\x1b[36mfoo\x1b[0m bar \x1b[31mbaz\x1b[0m"
// wrapped with selection bg
//   "\x1b[48;5;238m" + line + "\x1b[49m"
// got rendered with bg only on "foo" because the inner "\x1b[0m"
// also reset the selection bg attribute. Visually: scattered grey
// rectangles instead of a uniform highlight band.
//
// After: every CSI-m sequence inside the selected segment whose
// params include 0 or 49 is followed by an automatic re-emit of
// the selection bg open code, so the bg holds end-to-end.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestReinjectSelectionBg_NoOpOnPlainText(t *testing.T) {
	got := reinjectSelectionBg("hello world")
	if got != "hello world" {
		t.Errorf("plain text mutated: %q", got)
	}
}

func TestReinjectSelectionBg_AfterFullReset(t *testing.T) {
	// "\x1b[0m" should be followed by selection bg re-emit.
	in := "\x1b[36mfoo\x1b[0m bar"
	got := reinjectSelectionBg(in)
	want := "\x1b[36mfoo\x1b[0m" + selectionBgOpen + " bar"
	if got != want {
		t.Errorf("after reset:\n got %q\nwant %q", got, want)
	}
}

func TestReinjectSelectionBg_AfterBareCSI(t *testing.T) {
	// Bare "\x1b[m" (no params) is a reset-all and must re-arm bg.
	in := "x\x1b[my"
	got := reinjectSelectionBg(in)
	want := "x\x1b[m" + selectionBgOpen + "y"
	if got != want {
		t.Errorf("after bare CSI:\n got %q\nwant %q", got, want)
	}
}

func TestReinjectSelectionBg_AfterBgOnlyReset(t *testing.T) {
	// "\x1b[49m" resets bg without touching fg/attrs.
	in := "x\x1b[49my"
	got := reinjectSelectionBg(in)
	want := "x\x1b[49m" + selectionBgOpen + "y"
	if got != want {
		t.Errorf("after [49m:\n got %q\nwant %q", got, want)
	}
}

func TestReinjectSelectionBg_AfterCompoundReset(t *testing.T) {
	// "\x1b[0;1;31m" includes 0 → resets all.
	in := "x\x1b[0;1;31my"
	got := reinjectSelectionBg(in)
	if !strings.Contains(got, "\x1b[0;1;31m"+selectionBgOpen+"y") {
		t.Errorf("compound 0;… not re-armed:\n got %q", got)
	}
}

func TestReinjectSelectionBg_LeavesNonResettingSGRAlone(t *testing.T) {
	// Pure fg-red \x1b[31m doesn't touch bg → no reinjection needed,
	// keeps the byte stream lean.
	in := "x\x1b[31my\x1b[32mz"
	got := reinjectSelectionBg(in)
	if got != in {
		t.Errorf("fg-only sequences should pass through:\n got %q\nwant %q", got, in)
	}
}

func TestReinjectSelectionBg_MultipleResets(t *testing.T) {
	// Two resets in one segment: bg should re-arm after EACH.
	in := "a\x1b[0mb\x1b[0mc"
	got := reinjectSelectionBg(in)
	count := strings.Count(got, selectionBgOpen)
	if count != 2 {
		t.Errorf("expected 2 bg re-arms after two resets, got %d in %q", count, got)
	}
}

func TestApplySelectionToLine_StyledLineKeepsBgUniformly(t *testing.T) {
	// Real-shape input — a chat line with two styled spans separated
	// by a plain space. Pre-fix: only the first span would carry the
	// selection bg, with everything after the first \x1b[0m showing
	// no highlight. Post-fix: bg is present at both ends of the
	// content, with re-arm SGR sequences in between.
	l := makeListWithItems("doesn't matter, we override sel below")
	plainLen := ansi.StringWidth("foo bar baz")
	l.sel = selectionState{
		Anchor:     SelectionPoint{ItemIdx: 0, Line: 0, Col: 0},
		Focus:      SelectionPoint{ItemIdx: 0, Line: 0, Col: plainLen},
		HasContent: true,
	}
	styled := "\x1b[36mfoo\x1b[0m bar \x1b[31mbaz\x1b[0m"
	got := l.applySelectionToLine(styled, 0, 0)

	// Must open with bg.
	if !strings.HasPrefix(got, selectionBgOpen) {
		t.Errorf("output should start with selection bg open: %q", got)
	}
	// Each \x1b[0m inside the line MUST be followed by the bg re-arm.
	if strings.Count(got, selectionBgOpen) < 3 {
		// 1 from the wrapper, 2 from the two inner resets = 3.
		t.Errorf("expected 3 selection bg openings (wrapper + 2 reinjections); got %d in %q",
			strings.Count(got, selectionBgOpen), got)
	}
	// Plain-text content must round-trip.
	if ansi.Strip(got) != "foo bar baz" {
		t.Errorf("plain content lost: %q", ansi.Strip(got))
	}
}
