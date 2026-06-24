package tui

import "testing"

// TestTokenTracker_SnapshotRestore — cost survives a resume: Snapshot the
// cumulative totals, Restore them into a fresh tracker, and /cost-relevant
// getters (Input/Output/Total) reflect the restored values immediately.
func TestTokenTracker_SnapshotRestore(t *testing.T) {
	var src tokenTracker
	src.add(17000, 42, 0, 100) // one turn
	src.add(5000, 30, 0, 200)  // another
	in, out, cc, cr := src.Snapshot()
	if in != 22000 || out != 72 || cr != 300 {
		t.Fatalf("snapshot wrong: in=%d out=%d cc=%d cr=%d", in, out, cc, cr)
	}

	var resumed tokenTracker
	resumed.Restore(in, out, cc, cr)
	if resumed.Input() != 22000 || resumed.Output() != 72 || resumed.CacheRead() != 300 {
		t.Errorf("restore wrong: in=%d out=%d cr=%d", resumed.Input(), resumed.Output(), resumed.CacheRead())
	}
	// Display total reflects restored values without needing Animate().
	if resumed.Total() != 22000+72 {
		t.Errorf("restored Total()=%d want %d", resumed.Total(), 22000+72)
	}
}
