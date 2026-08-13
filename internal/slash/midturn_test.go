package slash

import "testing"

// TestClassifyMidTurn_Destructive — every signal that mutates loop
// state or expects to BE the start of a turn must be marked
// destructive. If a new signal that interrupts the loop is added
// without being added here, mid-turn input would silently corrupt
// the running turn.
func TestClassifyMidTurn_Destructive(t *testing.T) {
	destructive := []Signal{
		SignalQuit,
		SignalClear,
		SignalNew,
		SignalCompact,
		SignalReload,
		SignalUndo,
		SignalRewind,
		SignalRetry,
		SignalBranch,
		SignalResume,
		SignalBatch,
		SignalPlan,
	}
	for _, s := range destructive {
		if got := ClassifyMidTurn(s); got != MidTurnDestructive {
			t.Errorf("Signal %d should be MidTurnDestructive; got %v", s, got)
		}
	}
}

// TestClassifyMidTurn_Custom — SignalCustomPrompt is the only one
// whose handler resolves a template into prompt text. Mid-turn
// dispatch routes that text through SteerInject instead of running
// the signal handler.
func TestClassifyMidTurn_Custom(t *testing.T) {
	if got := ClassifyMidTurn(SignalCustomPrompt); got != MidTurnCustom {
		t.Errorf("SignalCustomPrompt should be MidTurnCustom; got %v", got)
	}
}

// TestClassifyMidTurn_DefaultsToSafe — anything not explicitly listed
// falls into MidTurnSafe (overlay-opening, theme-changing, etc. that
// don't touch Loop.Messages). Verify with a sample of the safe
// signals.
func TestClassifyMidTurn_DefaultsToSafe(t *testing.T) {
	safe := []Signal{
		SignalNone,
		SignalCost,
		SignalContext,
		SignalTools,
		SignalSessions,
		SignalStatus,
		SignalVersion,
		SignalEffort,
		SignalTheme,
		SignalPermissions,
		SignalHistory,
		SignalDoctor,
	}
	for _, s := range safe {
		if got := ClassifyMidTurn(s); got != MidTurnSafe {
			t.Errorf("Signal %d should default to MidTurnSafe; got %v", s, got)
		}
	}
}
