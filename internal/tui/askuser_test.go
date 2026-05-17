package tui

// AskUser blocking prompt — keybind handler + reply-channel contract.
// Mirrors overlays_baseline_test.go's permission-prompt coverage but
// exercises the variant case set (numeric shortcuts, freeform toggle,
// Esc dismiss surfaces as empty answer).

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func newAskUserTestModel(t *testing.T) *Model {
	m := newSlashTestModel(t)
	m.askUserActive = true
	m.askUserQuestion = "Which library should we use?"
	m.askUserOptions = []string{"opt one", "opt two", "opt three"}
	m.askUserAllowFreeform = true
	m.askUserCursor = 0
	m.askUserFreeformOn = false
	m.askUserInput = newAskUserInput()
	return m
}

func TestAskUser_NumericShortcutPicksOption(t *testing.T) {
	m := newAskUserTestModel(t)
	reply := make(chan string, 1)
	m.askUserReply = reply

	m.handleAskUserKey(tea.KeyPressMsg{Text: "2", Code: '2'})

	select {
	case ans := <-reply:
		if ans != "opt two" {
			t.Errorf("numeric `2` should pick opt two, got %q", ans)
		}
	default:
		t.Fatalf("no answer sent through reply channel")
	}
	if m.askUserActive {
		t.Errorf("askUserActive should clear after pick")
	}
}

func TestAskUser_OutOfRangeNumberDoesNotSubmit(t *testing.T) {
	m := newAskUserTestModel(t)
	reply := make(chan string, 1)
	m.askUserReply = reply

	// "5" with only 3 options should NOT submit.
	_, _, handled := m.handleAskUserKey(tea.KeyPressMsg{Text: "5", Code: '5'})
	if !handled {
		t.Errorf("out-of-range digit should still be handled (consumed, not fallthrough)")
	}
	select {
	case ans := <-reply:
		t.Fatalf("out-of-range digit should not submit, got %q", ans)
	default:
	}
	if !m.askUserActive {
		t.Errorf("askUserActive should remain true after invalid pick")
	}
}

func TestAskUser_ArrowsNavigate(t *testing.T) {
	m := newAskUserTestModel(t)
	m.askUserReply = make(chan string, 1)

	m.handleAskUserKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.askUserCursor != 1 {
		t.Errorf("↓ should move cursor to 1, got %d", m.askUserCursor)
	}
	m.handleAskUserKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.askUserCursor != 2 {
		t.Errorf("↓↓ should move cursor to 2, got %d", m.askUserCursor)
	}
	// Bottom clamp.
	m.handleAskUserKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.askUserCursor != 2 {
		t.Errorf("↓ at bottom should clamp, got %d", m.askUserCursor)
	}
	m.handleAskUserKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.askUserCursor != 1 {
		t.Errorf("↑ should retreat to 1, got %d", m.askUserCursor)
	}
}

func TestAskUser_EnterOnOptionPicksIt(t *testing.T) {
	m := newAskUserTestModel(t)
	reply := make(chan string, 1)
	m.askUserReply = reply
	m.askUserCursor = 2

	m.handleAskUserKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	select {
	case ans := <-reply:
		if ans != "opt three" {
			t.Errorf("Enter on cursor=2 should pick opt three, got %q", ans)
		}
	default:
		t.Fatalf("Enter should submit picked option")
	}
}

func TestAskUser_EscDismissesWithEmpty(t *testing.T) {
	m := newAskUserTestModel(t)
	reply := make(chan string, 1)
	m.askUserReply = reply

	m.handleAskUserKey(tea.KeyPressMsg{Code: tea.KeyEscape})

	select {
	case ans := <-reply:
		if ans != "" {
			t.Errorf("Esc should send empty answer (signal dismiss), got %q", ans)
		}
	default:
		t.Fatalf("Esc should still send through reply channel")
	}
	if m.askUserActive {
		t.Errorf("askUserActive should clear after Esc")
	}
}

func TestAskUser_TabTogglesFreeform(t *testing.T) {
	m := newAskUserTestModel(t)
	m.askUserReply = make(chan string, 1)

	if m.askUserFreeformOn {
		t.Fatalf("freeform should start off when options exist")
	}
	m.handleAskUserKey(tea.KeyPressMsg{Code: tea.KeyTab})
	if !m.askUserFreeformOn {
		t.Errorf("Tab should move focus into freeform input")
	}
	// Tab back to list.
	m.handleAskUserKey(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.askUserFreeformOn {
		t.Errorf("Second Tab should move focus back to option list")
	}
}

func TestAskUser_FreeformEnterSubmitsTypedValue(t *testing.T) {
	m := newAskUserTestModel(t)
	reply := make(chan string, 1)
	m.askUserReply = reply
	m.askUserFreeformOn = true
	m.askUserInput.Focus()
	m.askUserInput.SetValue("none of the above — write tests instead")

	m.handleAskUserKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	select {
	case ans := <-reply:
		if ans != "none of the above — write tests instead" {
			t.Errorf("freeform Enter should submit typed value, got %q", ans)
		}
	default:
		t.Fatalf("freeform Enter should submit typed answer")
	}
}

func TestAskUser_NoOptionsForcesFreeformFocus(t *testing.T) {
	m := newAskUserTestModel(t)
	m.askUserOptions = nil
	m.askUserFreeformOn = true
	m.askUserAllowFreeform = true
	reply := make(chan string, 1)
	m.askUserReply = reply
	m.askUserInput.SetValue("user-typed")
	m.askUserInput.Focus()

	m.handleAskUserKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	select {
	case ans := <-reply:
		if ans != "user-typed" {
			t.Errorf("expected typed answer, got %q", ans)
		}
	default:
		t.Fatalf("Enter in freeform mode should submit")
	}
}

func TestRenderAskUser_ShowsQuestionAndOptions(t *testing.T) {
	m := newAskUserTestModel(t)
	m.width = 80

	out := renderAskUser(m)
	if out == "" {
		t.Fatal("renderAskUser produced empty output")
	}
	wantSubstrings := []string{
		"AskUser",
		"Which library",
		"1.",
		"opt one",
		"opt two",
		"opt three",
		"Type your own answer",
	}
	for _, want := range wantSubstrings {
		if !containsStripped(out, want) {
			t.Errorf("renderAskUser output missing %q\n---\n%s", want, out)
		}
	}
}

// containsStripped checks if `haystack` contains `needle` after
// stripping ANSI escapes — render output is styled, but the test
// only cares about the visible text.
func containsStripped(haystack, needle string) bool {
	stripped := stripANSITest(haystack)
	for i := 0; i+len(needle) <= len(stripped); i++ {
		if stripped[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func stripANSITest(s string) string {
	out := make([]byte, 0, len(s))
	i := 0
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			// skip until letter (CSI end)
			j := i + 2
			for j < len(s) {
				c := s[j]
				if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
					j++
					break
				}
				j++
			}
			i = j
			continue
		}
		out = append(out, s[i])
		i++
	}
	return string(out)
}
