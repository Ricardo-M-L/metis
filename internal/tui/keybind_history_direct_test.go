package tui

import "testing"

func TestDirectHistory_EmptyInputEligible(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.histDirectIdx = -1
	if !m.directHistoryEligible() {
		t.Error("empty input should be eligible for direct history nav")
	}
}

func TestDirectHistory_NonEmptyInputNotEligible(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.histDirectIdx = -1
	m.input.SetValue("typed something")
	if m.directHistoryEligible() {
		t.Error("non-empty input with no nav-mode should NOT be eligible (textarea handles ↑↓)")
	}
}

func TestDirectHistory_InNavModeEligibleEvenIfTextSet(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.histDirectIdx = 0
	m.input.SetValue("loaded from history")
	if !m.directHistoryEligible() {
		t.Error("once in nav mode, ↑↓ stays eligible even though input is non-empty")
	}
}

func TestDirectHistory_UpFromEmptyLoadsLatest(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	// Skip filesystem load by pre-populating histAll
	m.histAll = []string{"latest", "older", "oldest"}
	m.histDirectIdx = -1

	if !m.directHistoryUp() {
		t.Fatal("first ↑ should succeed")
	}
	if m.histDirectIdx != 0 {
		t.Errorf("first ↑ should land at index 0; got %d", m.histDirectIdx)
	}
	if got := m.input.Value(); got != "latest" {
		t.Errorf("input should be 'latest'; got %q", got)
	}
}

func TestDirectHistory_UpClampsAtOldest(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.histAll = []string{"a", "b"}
	m.histDirectIdx = -1

	m.directHistoryUp() // → "a", idx 0
	m.directHistoryUp() // → "b", idx 1
	m.directHistoryUp() // clamp at 1
	m.directHistoryUp() // clamp at 1

	if m.histDirectIdx != 1 {
		t.Errorf("idx should clamp at oldest (1); got %d", m.histDirectIdx)
	}
	if got := m.input.Value(); got != "b" {
		t.Errorf("clamped input should still be 'b'; got %q", got)
	}
}

func TestDirectHistory_DownRestoresDraft(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.histAll = []string{"old1", "old2"}
	m.histDirectIdx = -1
	m.input.SetValue("draft in progress")

	m.directHistoryUp() // saves draft, loads old1
	if m.histDirectDraft != "draft in progress" {
		t.Errorf("draft should be saved on first ↑; got %q", m.histDirectDraft)
	}

	if !m.directHistoryDown() {
		t.Fatal("↓ at idx 0 should succeed")
	}
	if m.histDirectIdx != -1 {
		t.Errorf("↓ past 0 should exit nav mode (idx=-1); got %d", m.histDirectIdx)
	}
	if got := m.input.Value(); got != "draft in progress" {
		t.Errorf("draft should be restored; got %q", got)
	}
}

func TestDirectHistory_EmptyHistoryNoOp(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.histAll = []string{} // explicitly empty (avoid lazy-load filesystem hit)
	m.histDirectIdx = -1
	if m.directHistoryUp() {
		t.Error("↑ with empty history should not claim to handle the key")
	}
}

func TestDirectHistory_DownWithoutNavFallsThrough(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.histDirectIdx = -1
	if m.directHistoryDown() {
		t.Error("↓ outside nav mode should fall through to textarea")
	}
}

func TestDirectHistory_ResetClearsDraftAndIdx(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.histDirectIdx = 2
	m.histDirectDraft = "saved"
	m.resetDirectHistoryNav()
	if m.histDirectIdx != -1 || m.histDirectDraft != "" {
		t.Errorf("reset should zero idx + draft; got idx=%d draft=%q",
			m.histDirectIdx, m.histDirectDraft)
	}
}
