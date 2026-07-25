package tui

// mode_badge_test.go — locks the bottom-of-screen mode indicator
// glyph + render behavior added in the 2026-05-11 claude-code parity
// pass (commit b5a023c).
//
// Two contracts being pinned:
//   1. modeIcon returns the exact glyphs the user sees, mapping
//      claude-code's PermissionMode.ts color/symbol pairs.
//   2. renderHints, for `ask` mode, renders ONLY the dim cycle hint —
//      no bold "ask mode on" badge. Every other mode renders the full
//      badge. The ask case is the visible signal that the mode is
//      "default-quiet" vs "explicitly cycled-into".

import (
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// TestModeIcon_GlyphsMatchClaudeCode pins each mode's glyph to the
// exact characters that match claude-code's PermissionMode.ts. A
// refactor that swaps these out (back to ⏩, or accidentally drops
// the empty for `ask`) will trip immediately.
func TestModeIcon_GlyphsMatchClaudeCode(t *testing.T) {
	cases := []struct {
		mode  string
		glyph string
		note  string
	}{
		{"default", "", "default mode renders without a badge (PermissionMode.ts:48)"},
		{"acceptEdits", "⏵⏵", "claude-code autoAccept double-chevron"},
		{"bypassPermissions", "⏵⏵", "claude-code bypassPermissions — same glyph as acceptEdits, red color (PermissionMode.ts:69)"},
		{"plan", "⏸ ", "pause icon"},
		{"dontAsk", "⏵⏵", "claude-code dontAsk double-chevron in error color"},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			got, _ := modeIcon(c.mode)
			if got != c.glyph {
				t.Errorf("modeIcon(%q) glyph = %q, want %q\n  reason: %s",
					c.mode, got, c.glyph, c.note)
			}
		})
	}
}

// TestModeIcon_BypassIsRed — bypass and deny share red because both
// telegraph "you've cycled into a dangerous mode." Without color
// differentiation, a user couldn't tell bypass from acceptEdits at a
// glance — they share the ⏵⏵ glyph by claude-code parity. The color
// is what carries the safety signal.
func TestModeIcon_BypassIsRed(t *testing.T) {
	_, col := modeIcon("bypassPermissions")
	// lipgloss.Color returns a color.Color; the user-visible hex is
	// what we care about. Format as hex and compare prefix.
	r, g, b, _ := col.RGBA()
	// #e57373 = (229, 115, 115) in 8-bit → (58853, 29555, 29555) in 16-bit RGBA.
	// Looser comparison so a future palette tweak doesn't trip this if
	// it stays in the "red" family.
	if r>>8 < 200 || g>>8 > 150 || b>>8 > 150 {
		t.Errorf("bypass color should be red-family; got rgba (%d,%d,%d)", r>>8, g>>8, b>>8)
	}
}

func TestModeIcon_DontAskIsRed(t *testing.T) {
	_, col := modeIcon("dontAsk")
	r, g, b, _ := col.RGBA()
	if r>>8 < 200 || g>>8 > 150 || b>>8 > 150 {
		t.Errorf("dontAsk color should be red-family; got rgba (%d,%d,%d)", r>>8, g>>8, b>>8)
	}
}

func TestModeCommandHelpUsesCanonicalNames(t *testing.T) {
	cmd := BuildREPLCommands().Get("mode")
	if cmd == nil {
		t.Fatal("mode command is not registered")
	}
	want := "show or set permission mode (default|acceptEdits|plan|dontAsk|bypassPermissions)"
	if cmd.Description != want {
		t.Fatalf("mode help = %q, want %q", cmd.Description, want)
	}
}

// renderHintsHelper builds a tiny Model with just the gate populated so
// renderHints can run without booting the full TUI. permActive is
// false (renderHints returns early when true).
func renderHintsHelper(t *testing.T, mode permission.Mode) string {
	t.Helper()
	m := &Model{
		gate:      permission.New(mode),
		startTime: time.Now(),
	}
	return renderHints(m)
}

// TestRenderHints_AskModeOmitsBadge — the key claude-code-parity move
// of commit b5a023c. In ask (= claude-code's default), the bottom
// hint line shows ONLY the dim "shift+tab to cycle modes" text — no
// bold mode name, no glyph, no "ask mode on" string.
func TestRenderHints_AskModeOmitsBadge(t *testing.T) {
	out := renderHintsHelper(t, permission.ModeAsk)
	if strings.Contains(out, "ask mode") {
		t.Errorf("ask mode hint should NOT include 'ask mode on' badge; got:\n%s", out)
	}
	if !strings.Contains(out, "shift+tab") {
		t.Errorf("ask mode hint should still surface the discovery cue; got:\n%s", out)
	}
	if !strings.Contains(out, "cycle") {
		t.Errorf("ask mode hint should surface 'cycle' verb; got:\n%s", out)
	}
}

// TestRenderHints_NonAskShowsBadge — counter-case: every other mode
// SHOULD render the bold badge. Lock the modal/inline distinction
// so a refactor can't accidentally hide the warning glyph for bypass.
func TestRenderHints_NonAskShowsBadge(t *testing.T) {
	cases := []struct {
		mode permission.Mode
		want string
	}{
		{permission.ModeAcceptEdits, "acceptEdits mode"},
		{permission.ModePlan, "plan mode"},
		{permission.ModeBypassPermissions, "bypassPermissions mode"},
		{permission.ModeDontAsk, "dontAsk mode"},
	}
	for _, c := range cases {
		t.Run(string(c.mode), func(t *testing.T) {
			out := renderHintsHelper(t, c.mode)
			if !strings.Contains(out, c.want) {
				t.Errorf("%s mode should show '%s' badge; got:\n%s",
					c.mode, c.want, out)
			}
			if !strings.Contains(out, "shift+tab") {
				t.Errorf("%s mode should still show cycle hint; got:\n%s",
					c.mode, out)
			}
		})
	}
}

// TestRenderHints_BypassCarriesGlyph — pin that bypass mode's bottom-
// of-screen badge actually carries the ⏵⏵ glyph the user expects.
// If glyph mapping changes (e.g. someone reverts to ⏩), this trips.
func TestRenderHints_BypassCarriesGlyph(t *testing.T) {
	out := renderHintsHelper(t, permission.ModeBypass)
	if !strings.Contains(out, "⏵⏵") {
		t.Errorf("bypass mode hint should contain ⏵⏵ glyph; got:\n%s", out)
	}
}

// TestRenderHints_PermPromptActiveSuppresses — when a permission
// prompt is mid-flight (m.permActive=true), the bottom hint is
// suppressed entirely so the permission dialog has the floor. Pin
// this so a refactor of permActive handling can't accidentally
// double-up the hint line on top of the prompt.
func TestRenderHints_PermPromptActiveSuppresses(t *testing.T) {
	m := &Model{
		gate:       permission.New(permission.ModeAsk),
		permActive: true,
	}
	if got := renderHints(m); got != "" {
		t.Errorf("renderHints during active perm prompt must be empty; got %q", got)
	}
}
