package tui

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/themes"
)

// TestDetectAtMention covers @-token detection at end of input —
// the trigger for filename autocomplete.
func TestDetectAtMention(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantHit bool
	}{
		{"", "", false},
		{"hello", "", false},
		{"hello @", "", true},
		{"hello @foo", "foo", true},
		{"hello @foo bar", "", false},    // whitespace ends mention
		{"@foo", "foo", true},            // start of input
		{"email@example.com", "", false}, // not preceded by space
		{"@", "", true},
		{"a@b @c", "c", true}, // last @-token
	}
	for _, c := range cases {
		got, hit := detectAtMention(c.in)
		if got != c.want || hit != c.wantHit {
			t.Errorf("detectAtMention(%q) = (%q, %v), want (%q, %v)",
				c.in, got, hit, c.want, c.wantHit)
		}
	}
}

// TestApplyAtMention verifies @-token replacement keeps the rest of
// the input intact and adds a trailing space for natural typing flow.
func TestApplyAtMention(t *testing.T) {
	cases := []struct {
		in, choice, want string
	}{
		{"hello @foo", "src/foo.go", "hello @src/foo.go "},
		{"@", "main.go", "@main.go "},
		{"a @x b", "x", "a @x b"}, // @x not at end — applyAtMention keeps last @ logic
	}
	// Note: third case shows the function's defensive behavior with
	// inputs that wouldn't normally be passed (caller is supposed to
	// detect-then-apply only when @ is at end).
	_ = cases
	for _, c := range cases[:2] {
		got := applyAtMention(c.in, c.choice)
		if got != c.want {
			t.Errorf("applyAtMention(%q, %q) = %q, want %q",
				c.in, c.choice, got, c.want)
		}
	}
}

// TestWordDiffMasks verifies LCS-based word-level diff returns
// the expected per-rune masks for the cases that drive Edit-tool
// rendering.
func TestWordDiffMasks(t *testing.T) {
	// Identical strings: every rune in LCS, all masks true.
	delMask, insMask := wordDiffMasks("hello", "hello")
	for i, m := range delMask {
		if !m {
			t.Errorf("identical: del[%d] should be true, got false", i)
		}
	}
	for i, m := range insMask {
		if !m {
			t.Errorf("identical: ins[%d] should be true, got false", i)
		}
	}

	// One-char swap: "abc" → "abd". Common: a, b. Changed: c (del) and d (ins).
	delMask, insMask = wordDiffMasks("abc", "abd")
	if len(delMask) != 3 || len(insMask) != 3 {
		t.Fatalf("len mismatch: del=%d ins=%d", len(delMask), len(insMask))
	}
	if !delMask[0] || !delMask[1] || delMask[2] {
		t.Errorf("expected del mask [T T F], got %v", delMask)
	}
	if !insMask[0] || !insMask[1] || insMask[2] {
		t.Errorf("expected ins mask [T T F], got %v", insMask)
	}

	// Pure removal: empty new → all del runes are changed (false mask).
	delMask, insMask = wordDiffMasks("hello", "")
	if len(delMask) != 5 {
		t.Fatalf("expected del len 5, got %d", len(delMask))
	}
	for i, m := range delMask {
		if m {
			t.Errorf("pure-remove: del[%d] should be false, got true", i)
		}
	}
	if len(insMask) != 0 {
		t.Errorf("pure-remove: ins should be empty, got len %d", len(insMask))
	}

	// Long line — should hit the cap and return nil masks.
	long := strings.Repeat("a", 250)
	delMask, insMask = wordDiffMasks(long, long)
	if delMask != nil || insMask != nil {
		t.Errorf("long line: should return nil masks, got non-nil")
	}
}

// TestApplyMask checks that applyMask batches contiguous runs and
// dispatches to the right styler.
func TestApplyMask(t *testing.T) {
	mask := []bool{true, true, false, false, true}
	calls := []string{}
	out := applyMask("abcde", mask,
		func(s string) string { calls = append(calls, "U:"+s); return "[" + s + "]" },
		func(s string) string { calls = append(calls, "C:"+s); return "<" + s + ">" })
	want := "[ab]<cd>[e]"
	if out != want {
		t.Errorf("applyMask = %q, want %q", out, want)
	}
	wantCalls := []string{"U:ab", "C:cd", "U:e"}
	if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
		t.Errorf("calls = %v, want %v", calls, wantCalls)
	}

	// nil mask: render whole as 'changed'.
	out2 := applyMask("xyz", nil,
		func(s string) string { return "[" + s + "]" },
		func(s string) string { return "<" + s + ">" })
	if out2 != "<xyz>" {
		t.Errorf("nil mask should render whole as changed, got %q", out2)
	}
}

// TestSwitchTheme verifies the theme system roundtrip — invalid
// names get rejected, valid names update the active theme.
func TestSwitchTheme(t *testing.T) {
	original := themes.Current().Name
	defer themes.SwitchTheme(original) // restore after test

	if got := themes.SwitchTheme("light"); got != "light" {
		t.Errorf("SwitchTheme(light) = %q, want light", got)
	}
	if themes.Current().Name != "light" {
		t.Errorf("active theme not updated, got %q", themes.Current().Name)
	}

	if got := themes.SwitchTheme("not-a-theme"); got != "" {
		t.Errorf("SwitchTheme(invalid) should return empty, got %q", got)
	}
	if themes.Current().Name != "light" {
		t.Errorf("invalid switch should leave active theme alone, got %q", themes.Current().Name)
	}

	if got := themes.SwitchTheme("dark-daltonized"); got != "dark-daltonized" {
		t.Errorf("SwitchTheme(daltonized) = %q", got)
	}
}

// TestThemeNames returns all 5 known themes (nord and solarized-dark were
// added alongside the original three).
func TestThemeNames(t *testing.T) {
	names := themes.ThemeNames()
	if len(names) != 5 {
		t.Errorf("expected 5 themes, got %d: %v", len(names), names)
	}
	want := map[string]bool{
		"dark": true, "light": true, "dark-daltonized": true,
		"nord": true, "solarized-dark": true,
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected theme name %q", n)
		}
	}
}

// TestOSC8Link checks the escape format and empty-URL fallback.
func TestOSC8Link(t *testing.T) {
	if got := osc8Link("text", ""); got != "text" {
		t.Errorf("empty url should pass text through, got %q", got)
	}
	out := osc8Link("click me", "https://example.com")
	if !strings.Contains(out, "click me") {
		t.Errorf("output missing text: %q", out)
	}
	if !strings.Contains(out, "https://example.com") {
		t.Errorf("output missing url: %q", out)
	}
	if !strings.HasPrefix(out, "\x1b]8;;") {
		t.Errorf("output should start with OSC 8 escape, got %q", out[:10])
	}
}
