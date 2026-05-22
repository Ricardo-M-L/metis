package agent

import "testing"

// TestBatchBounds_SaneRange — guard the constants stay in a
// reasonable range. If MIN > MAX or either drops below 1, the
// prompt + Agent description copy is wrong and a model reading them
// would get confusing instructions. Cheap regression catcher for a
// future "I'll just lower MAX to 5" thinko.
func TestBatchBounds_SaneRange(t *testing.T) {
	if BatchMinUnits < 1 {
		t.Errorf("BatchMinUnits = %d, must be >= 1", BatchMinUnits)
	}
	if BatchMaxUnits <= BatchMinUnits {
		t.Errorf("BatchMaxUnits (%d) must be > BatchMinUnits (%d)", BatchMaxUnits, BatchMinUnits)
	}
	// Sanity: MAX should fit in Roster.capAnon (default 40) so a
	// single fan-out can dispatch the full max without filling the
	// anon pool. If anyone bumps MAX above 40, they need to also
	// bump capAnon or split into multiple turns.
	if BatchMaxUnits > 40 {
		t.Errorf("BatchMaxUnits (%d) exceeds default capAnon=40; would deadlock fan-outs", BatchMaxUnits)
	}
}

// TestBatchBounds_MatchClaudeCode — claude-code's batch.ts uses
// MIN_AGENTS=5, MAX_AGENTS=30. We deliberately keep parity to avoid
// model confusion (deepseek + others have seen claude-code prompts
// in training; matching numbers means "decompose into 5-30 units"
// is the same instruction both tools give). Document the parity
// here so a future maintainer doesn't drift the values silently.
func TestBatchBounds_MatchClaudeCode(t *testing.T) {
	if BatchMinUnits != 5 {
		t.Errorf("BatchMinUnits = %d; claude-code uses 5 — change requires updating prompt copy", BatchMinUnits)
	}
	if BatchMaxUnits != 30 {
		t.Errorf("BatchMaxUnits = %d; claude-code uses 30 — change requires updating prompt copy", BatchMaxUnits)
	}
}
