package slash

import (
	"strings"
	"testing"
)

// TestSlashThinking_ValidArgs_EmitsSignal — pins the /thinking
// dispatch. All three accepted modes (show/hide/auto) must produce
// SignalThinkingDisplay so the TUI's keybind_submit handler can flip
// m.thinkingDisplay accordingly. Without this contract a user typing
// /thinking show silently no-ops.
func TestSlashThinking_ValidArgs_EmitsSignal(t *testing.T) {
	r := NewRegistry()
	RegisterAll(r, nil)
	for _, arg := range []string{"show", "hide", "auto"} {
		handled, _, sig, _ := r.Parse("/thinking " + arg)
		if !handled {
			t.Errorf("/thinking %s not handled by registry", arg)
		}
		if sig != SignalThinkingDisplay {
			t.Errorf("/thinking %s sig = %v, want SignalThinkingDisplay", arg, sig)
		}
	}
}

// TestSlashThinking_NoArg_PrintsUsage — bare /thinking (no arg)
// returns the usage hint instead of silently failing. Lets the user
// discover the three modes by typing the bare command.
func TestSlashThinking_NoArg_PrintsUsage(t *testing.T) {
	r := NewRegistry()
	RegisterAll(r, nil)
	handled, display, sig, _ := r.Parse("/thinking")
	if !handled {
		t.Fatalf("/thinking (no arg) not handled")
	}
	if sig != SignalNone {
		t.Errorf("bare /thinking should emit SignalNone (just print usage); got %v", sig)
	}
	for _, want := range []string{"show", "hide", "auto"} {
		if !strings.Contains(display, want) {
			t.Errorf("usage text missing %q: %s", want, display)
		}
	}
}

// TestSlashThinking_InvalidArg_RejectsCleanly — a typo like
// /thinking expand must show "unknown mode" and emit SignalNone
// rather than flipping to an unrecognised state.
func TestSlashThinking_InvalidArg_RejectsCleanly(t *testing.T) {
	r := NewRegistry()
	RegisterAll(r, nil)
	_, display, sig, _ := r.Parse("/thinking expand")
	if sig != SignalNone {
		t.Errorf("invalid arg should emit SignalNone, got %v", sig)
	}
	if !strings.Contains(display, "unknown mode") || !strings.Contains(display, "expand") {
		t.Errorf("usage rejection should echo the bad arg: %q", display)
	}
}
