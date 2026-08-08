package tui

import (
	"strings"
	"testing"
	"time"
)

// TestRender_SuccessRoleAddsCheckGlyph — Phase B introduces "success"
// role for celebratory confirmations (saved, exported, branched, undid).
// The renderer must add a ✓ glyph so the user gets visible "this
// landed" feedback distinct from neutral info messages.
func TestRender_SuccessRoleAddsCheckGlyph(t *testing.T) {
	msg := Message{Role: "success", Content: "session synced to disk", Timestamp: time.Now()}
	out := stripANSI(renderMessage(msg, 80, false))
	if !strings.Contains(out, "✓") {
		t.Errorf("success role should render with ✓ glyph; got: %q", out)
	}
	if !strings.Contains(out, "session synced") {
		t.Errorf("success body missing; got: %q", out)
	}
}

// TestRender_WarningRoleAddsWarnGlyph — "warning" role gets ⚠ in
// orange for soft warnings (no session store, deprecated usage).
func TestRender_WarningRoleAddsWarnGlyph(t *testing.T) {
	msg := Message{Role: "warning", Content: "no session store available", Timestamp: time.Now()}
	out := stripANSI(renderMessage(msg, 80, false))
	if !strings.Contains(out, "⚠") {
		t.Errorf("warning role should render with ⚠ glyph; got: %q", out)
	}
}

// TestRender_InfoRoleAddsBulletGlyph — Phase B also adds a "·" prefix
// to plain info messages so the left edge aligns with success / error
// / warning. Pre-fix the info rows floated unaligned against the
// glyph-prefixed neighbors.
func TestRender_InfoRoleAddsBulletGlyph(t *testing.T) {
	msg := Message{Role: "info", Content: "history cleared", Timestamp: time.Now()}
	out := stripANSI(renderMessage(msg, 80, false))
	if !strings.Contains(out, "·") {
		t.Errorf("info role should render with · prefix; got: %q", out)
	}
}

// TestRender_ErrorRoleStillHasXGlyph — regression guard: existing
// error rendering must not change.
func TestRender_ErrorRoleStillHasXGlyph(t *testing.T) {
	msg := Message{Role: "error", Content: "failed to write", Timestamp: time.Now()}
	out := stripANSI(renderMessage(msg, 80, false))
	if !strings.Contains(out, "✗") {
		t.Errorf("error role should keep ✗ glyph; got: %q", out)
	}
}

func TestRender_CommandResultUsesClaudeTreeLeaf(t *testing.T) {
	msg := Message{Role: "command-result", Content: "Conversation exported to: /tmp/session.txt", Timestamp: time.Now()}
	out := stripANSI(renderMessage(msg, 100, false))
	if !strings.Contains(out, "⎿  Conversation exported to:") {
		t.Fatalf("command result should use Claude-style tree leaf; got: %q", out)
	}
	if strings.Contains(out, "✓") {
		t.Fatalf("command result must not use the generic success check; got: %q", out)
	}
}

// TestRender_AllStatusRolesDistinguishable — under stripANSI the four
// status rows have visually distinct prefixes. Catches a future patch
// that copy-pastes the wrong glyph into the wrong case.
func TestRender_AllStatusRolesDistinguishable(t *testing.T) {
	prefixes := map[string]string{
		"info":    "·",
		"success": "✓",
		"warning": "⚠",
		"error":   "✗",
	}
	seen := map[string]bool{}
	for role, want := range prefixes {
		out := stripANSI(renderMessage(Message{Role: role, Content: "hello", Timestamp: time.Now()}, 80, false))
		if !strings.Contains(out, want) {
			t.Errorf("role=%q missing prefix %q; got: %q", role, want, out)
		}
		if seen[want] {
			t.Errorf("prefix %q used by multiple roles", want)
		}
		seen[want] = true
	}
}
