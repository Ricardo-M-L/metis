package tasks

import "testing"

// TestNormalizeTaskContent_MergesNumberingPrefixes — image #50 repro:
// "Cluster 2: Wire Protocol (...)" and "2. Wire Protocol (...)"
// should normalise to the same string so the dedup catches them as
// one task even when the model re-wrote the label.
func TestNormalizeTaskContent_MergesNumberingPrefixes(t *testing.T) {
	pairs := []struct {
		a, b string
		want bool // true → should normalise equal
	}{
		// image #50 exact pair
		{"Cluster 2: Wire Protocol (types, file, serde, wire, protocol, server)",
			"2. Wire Protocol (types, file, serde, wire, protocol, server)", true},
		// More numbering-prefix flavors
		{"Phase 1: build foo", "1. build foo", true},
		{"Step 3: refactor X", "3) refactor X", true},
		{"Part 2: write tests", "[2] write tests", true},
		// Trailing ellipsis (… and ...)
		{"do the thing…", "do the thing", true},
		{"do the thing...", "do the thing", true},
		// Case + whitespace insensitive
		{"Cluster 1: Foo", "cluster 1:  foo", true},
		// Different meaningful bodies → MUST stay distinct (don't over-merge)
		{"Cluster 1: Foundation", "Cluster 1: exception, constant", false},
		{"Phase 1: build", "Phase 2: build", true /* numbering stripped → same body */},
		{"refactor auth", "refactor db", false},
		// Plain numbers that aren't really numbering — shouldn't strip
		{"5-second timeout window", "10-second timeout window", false},
	}
	for _, p := range pairs {
		na, nb := normalizeTaskContent(p.a), normalizeTaskContent(p.b)
		got := na == nb
		if got != p.want {
			t.Errorf("normalise(%q) == normalise(%q):\n  got equal=%v (a→%q, b→%q)\n  want equal=%v",
				p.a, p.b, got, na, nb, p.want)
		}
	}
}

// TestUpsert_NormalizedDedup — end-to-end check via Upsert:
// model creates "Cluster 2: Wire Protocol" then later calls with
// "2. Wire Protocol" (no id) — must update the SAME row, not
// append a second. Image #50 mistakenly appended.
func TestUpsert_NormalizedDedup(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	sid := "s-dedup"

	first, err := Upsert(sid, Item{Content: "Cluster 2: Wire Protocol (types, file, serde, wire)"})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Same task, different label — no id supplied.
	second, err := Upsert(sid, Item{Content: "2. Wire Protocol (types, file, serde, wire)", Status: "in_progress"})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	// MUST update first instead of creating a new one.
	if second.ID != first.ID {
		t.Errorf("dedup failed: second.ID=%q first.ID=%q — duplicate created", second.ID, first.ID)
	}
	if second.Status != "in_progress" {
		t.Errorf("status update lost: got %q", second.Status)
	}
	// Verify on disk only ONE item exists.
	tl, err := Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(tl.Items); got != 1 {
		t.Errorf("expected 1 task after normalized-dedup; got %d (%+v)", got, tl.Items)
	}
}

// TestUpsert_DoesNotMergeDifferentBodies — guardrail: the dedup
// must NOT merge two tasks that share a prefix but have genuinely
// different bodies (image #50 also had "Cluster 1: Foundation" vs
// "Cluster 1: exception, ..." which ARE different tasks).
func TestUpsert_DoesNotMergeDifferentBodies(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	sid := "s-distinct"

	a, _ := Upsert(sid, Item{Content: "Cluster 1: Foundation (exception, constant, share)"})
	b, _ := Upsert(sid, Item{Content: "Cluster 1: exception, constant, share, metadata, session/state"})
	if a.ID == b.ID {
		t.Errorf("over-merge: distinct task bodies got merged: %q vs %q", a.Content, b.Content)
	}
	tl, _ := Load(sid)
	if got := len(tl.Items); got != 2 {
		t.Errorf("expected 2 distinct tasks; got %d", got)
	}
}
