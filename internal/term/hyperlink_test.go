package term

import (
	"strings"
	"testing"
)

func TestHyperlink_SupportedTerminal(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTermEnv(t)
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	got := Hyperlink("click here", "https://example.com")
	if !strings.Contains(got, "\x1b]8;id=") {
		t.Errorf("missing OSC 8 prefix: %q", got)
	}
	if !strings.Contains(got, "https://example.com") {
		t.Errorf("missing url: %q", got)
	}
	if !strings.Contains(got, "click here") {
		t.Errorf("missing text: %q", got)
	}
	if !strings.HasSuffix(got, "\x1b]8;;\x07") {
		t.Errorf("missing close: %q", got)
	}
}

func TestHyperlink_UnsupportedTerminalFallsBack(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTermEnv(t)
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	got := Hyperlink("click here", "https://example.com")
	want := "click here (https://example.com)"
	if got != want {
		t.Errorf("fallback: %q, want %q", got, want)
	}
}

func TestHyperlink_EmptyURLPassesThrough(t *testing.T) {
	if got := Hyperlink("just text", ""); got != "just text" {
		t.Errorf("empty url: %q", got)
	}
}

func TestHyperlink_SameURLSameID(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTermEnv(t)
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	a := Hyperlink("a", "https://x")
	b := Hyperlink("b", "https://x")
	idA := strings.Split(strings.TrimPrefix(a, "\x1b]8;id="), ";")[0]
	idB := strings.Split(strings.TrimPrefix(b, "\x1b]8;id="), ";")[0]
	if idA != idB {
		t.Errorf("same url should yield same id, got %q vs %q", idA, idB)
	}
}

func TestOSC8ID_StableHexLength(t *testing.T) {
	id := osc8ID("https://example.com/path?q=1")
	if len(id) != 8 {
		t.Errorf("id len = %d, want 8", len(id))
	}
}
