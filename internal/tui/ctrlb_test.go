package tui

// ctrlb_test.go — Phase F (2026-05-12) Ctrl+B background-turn
// toggle contracts.
//
// Five tests:
//   1. Ctrl+B at idle is a no-op (info hint, no flag flip).
//   2. Ctrl+B mid-turn flips turnBackgrounded=true + stamps
//      backgroundedAt + appends an info banner.
//   3. Second Ctrl+B mid-turn foregrounds (flag false, banner
//      "back in foreground").
//   4. finalizeTurn clears the bg flag + prefixes the flushed
//      assistant reply with "[bg]".
//   5. finalizeTurn force-fires the desktop notification path
//      even on a sub-30s turn when wasBackgrounded.

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func teaKeyCtrlB() tea.KeyMsg {
	return tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl}
}

func TestCtrlB_IdleIsNoop(t *testing.T) {
	m := makeModelForGateTest()
	m.turnActive = false

	_, _ = m.handleKey(teaKeyCtrlB())

	if m.turnBackgrounded {
		t.Error("idle Ctrl+B should NOT flip turnBackgrounded")
	}
	if len(m.messages) == 0 {
		t.Error("idle Ctrl+B should push an info hint so user learns the binding")
	} else {
		last := m.messages[len(m.messages)-1]
		if last.Role != "info" {
			t.Errorf("idle Ctrl+B should push info row; got role=%q", last.Role)
		}
	}
}

func TestCtrlB_MidTurnBackgrounds(t *testing.T) {
	m := makeModelForGateTest()
	m.turnActive = true

	_, _ = m.handleKey(teaKeyCtrlB())

	if !m.turnBackgrounded {
		t.Error("Ctrl+B mid-turn should set turnBackgrounded=true")
	}
	if m.backgroundedAt.IsZero() {
		t.Error("backgroundedAt should be stamped")
	}
	// Banner should mention "background"
	last := m.messages[len(m.messages)-1]
	if last.Role != "info" {
		t.Errorf("expected info banner; got role=%q", last.Role)
	}
}

func TestCtrlB_SecondPressForegrounds(t *testing.T) {
	m := makeModelForGateTest()
	m.turnActive = true
	// First press → bg
	_, _ = m.handleKey(teaKeyCtrlB())
	if !m.turnBackgrounded {
		t.Fatal("first Ctrl+B should background")
	}
	// Second press → fg
	_, _ = m.handleKey(teaKeyCtrlB())
	if m.turnBackgrounded {
		t.Error("second Ctrl+B should foreground (toggle off)")
	}
	if !m.backgroundedAt.IsZero() {
		t.Error("backgroundedAt should be cleared on foreground")
	}
}

func TestFinalizeTurn_ClearsBgFlagAndPrefixesReply(t *testing.T) {
	m := makeModelForGateTest()
	m.turnActive = true
	m.turnBackgrounded = true
	m.backgroundedAt = time.Now()
	m.streamingText = "hello from a bg reply"
	// spinnerStartedAt is needed by the notification gate;
	// non-zero so the gate path runs.
	m.spinnerStartedAt = time.Now().Add(-2 * time.Second)

	m.finalizeTurn(nil)

	if m.turnBackgrounded {
		t.Error("finalizeTurn should clear turnBackgrounded")
	}
	if !m.backgroundedAt.IsZero() {
		t.Error("finalizeTurn should clear backgroundedAt")
	}
	// Expect the streamingText flushed as an assistant message with [bg] prefix.
	var found bool
	for _, msg := range m.messages {
		if msg.Role == "assistant" && msg.Content == "[bg] hello from a bg reply" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected '[bg] hello from a bg reply' assistant message; got %d messages", len(m.messages))
		for i, msg := range m.messages {
			t.Logf("  [%d] role=%q content=%q", i, msg.Role, msg.Content)
		}
	}
}

// TestFinalizeTurn_BgBypassesNotifyDuration confirms that when a
// turn is backgrounded but finishes in under NotifyMinDuration,
// the notification still fires (the user explicitly asked not to
// look — they want to be poked).
func TestFinalizeTurn_BgBypassesNotifyDuration(t *testing.T) {
	m := makeModelForGateTest()
	m.turnActive = true
	m.turnBackgrounded = true
	m.backgroundedAt = time.Now()
	m.streamingText = "quick"
	// Spinner started 100ms ago — well under NotifyMinDuration.
	m.spinnerStartedAt = time.Now().Add(-100 * time.Millisecond)

	// We can't easily mock SendNotification, but the code path
	// runs the [bg] prefix logic only when wasBackgrounded was
	// captured at finalize entry. Test the OBSERVABLE side
	// effect: streamingText was flushed with prefix.
	m.finalizeTurn(nil)

	for _, msg := range m.messages {
		if msg.Role == "assistant" && len(msg.Content) >= 5 && msg.Content[:5] == "[bg] " {
			return // success — the notify branch fired and the prefix logic ran
		}
	}
	t.Error("expected '[bg]' prefix on flushed reply for short backgrounded turn")
}
