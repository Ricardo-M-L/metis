package tui

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/runtime"
)

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

// TestDirectHistory_FiltersBySessionID — user expectation 2026-05-16
// "向上向下箭头出来的是当前会话的吧不会出来别的会话的吧". When the
// model has a non-empty sessionID, loadDirectHistory must drop entries
// whose SessionID doesn't match — the global history file holds
// prompts from ALL sessions and we don't want cross-session bleed
// into ↑/↓ direct nav.
func TestDirectHistory_FiltersBySessionID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("METIS_HOME", tmp)
	t.Setenv("HOME", tmp) // belt-and-braces in case config.Home() falls back

	// Seed the on-disk history with prompts from two different sessions.
	for _, e := range []runtime.HistoryEntry{
		{SessionID: "other-sess", Input: "from-other-session-1"},
		{SessionID: "my-sess", Input: "mine-1"},
		{SessionID: "other-sess", Input: "from-other-session-2"},
		{SessionID: "my-sess", Input: "mine-2"},
	} {
		if err := runtime.AppendHistory(e); err != nil {
			t.Fatalf("seed AppendHistory: %v", err)
		}
	}

	m := newE2EModel(t, 120, 30, 0)
	m.sessionID = "my-sess"
	m.histAll = nil // force lazy-load to actually fire
	m.loadDirectHistory()

	// LoadRecentHistory returns newest first, so "mine-2" comes before
	// "mine-1". Other-session entries must be entirely absent.
	want := []string{"mine-2", "mine-1"}
	if len(m.histAll) != len(want) {
		t.Fatalf("histAll length: want %d, got %d (entries: %v)",
			len(want), len(m.histAll), m.histAll)
	}
	for i, w := range want {
		if m.histAll[i] != w {
			t.Errorf("histAll[%d]: want %q, got %q", i, w, m.histAll[i])
		}
	}
	for _, h := range m.histAll {
		if h == "from-other-session-1" || h == "from-other-session-2" {
			t.Errorf("cross-session entry leaked: %q", h)
		}
	}
}

// TestDirectHistory_NoSessionFallsBackToAllEntries — REPL fallback path
// has no session bound (sessionID == ""); filtering would yield an
// empty list and silently break ↑. Confirm the fallback returns the
// unfiltered view.
func TestDirectHistory_NoSessionFallsBackToAllEntries(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("METIS_HOME", tmp)
	t.Setenv("HOME", tmp)

	for _, e := range []runtime.HistoryEntry{
		{SessionID: "s1", Input: "p1"},
		{SessionID: "s2", Input: "p2"},
	} {
		if err := runtime.AppendHistory(e); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	m := newE2EModel(t, 120, 30, 0)
	m.sessionID = "" // no session
	m.histAll = nil
	m.loadDirectHistory()

	if len(m.histAll) != 2 {
		t.Fatalf("no-session fallback should see all 2 entries; got %d: %v",
			len(m.histAll), m.histAll)
	}
}
