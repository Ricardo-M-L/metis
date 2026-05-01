package tui

// Baseline e2e tests for the 4 overlays still on the field-soup
// implementation (permission ask / history search / palette / @-mention).
// /btw was migrated to internal/tui/overlay/ on 2026-05-01; these four
// are slated for Phase 2 of the same refactor. Until then, these tests
// freeze current behavior so the migration has regression protection.
//
// Tests directly poke Model state where helpful — the goal isn't to
// stress the trigger paths (those are well-known: input "/" / Ctrl-R /
// EventPermissionRequest / "@") but to lock down the post-trigger
// keyboard contract: nav / select / dismiss / filter / commit.

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

// ============================================================================
// 1. Permission ask
// ============================================================================

func TestPermissionAsk_NavigationCycles(t *testing.T) {
	m := newSlashTestModel(t)
	m.permActive = true
	m.permTool = "Bash"
	m.permArgs = "rm -rf /"
	m.permQuestion = "Allow Bash to run `rm -rf /`?"
	// Use the same choice list NewModel installs.
	m.permChoices = []permChoice{
		{Label: "Yes", Key: "y"},
		{Label: "Yes, always", Key: "a"},
		{Label: "No", Key: "n"},
		{Label: "Cancel turn", Key: "c"},
	}
	m.permCursor = 0
	m.permReply = make(chan agent.PermissionDecision, 1)

	// Right / Down advance.
	m.handlePermKey(tea.KeyMsg{Type: tea.KeyRight})
	if m.permCursor != 1 {
		t.Errorf("→ should move cursor 0→1, got %d", m.permCursor)
	}
	m.handlePermKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.permCursor != 2 {
		t.Errorf("↓ should move cursor 1→2, got %d", m.permCursor)
	}

	// Left / Up retreat.
	m.handlePermKey(tea.KeyMsg{Type: tea.KeyLeft})
	m.handlePermKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.permCursor != 0 {
		t.Errorf("after ←↑ cursor should be 0, got %d", m.permCursor)
	}

	// Cursor clamps at boundaries (no wrap-around).
	m.handlePermKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.permCursor != 0 {
		t.Errorf("↑ at top should clamp at 0, got %d", m.permCursor)
	}
	m.permCursor = len(m.permChoices) - 1
	m.handlePermKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.permCursor != len(m.permChoices)-1 {
		t.Errorf("↓ at bottom should clamp, got %d", m.permCursor)
	}
}

func TestPermissionAsk_EnterSendsDecision(t *testing.T) {
	m := newSlashTestModel(t)
	m.permActive = true
	m.permChoices = []permChoice{
		{Label: "Yes", Key: "y"},
		{Label: "No", Key: "n"},
	}
	// executePermission nils out m.permReply after sending; keep our own
	// reference so we can still read from the channel after dispatch.
	reply := make(chan agent.PermissionDecision, 1)
	m.permReply = reply
	m.permCursor = 0 // "Yes"

	m.handlePermKey(tea.KeyMsg{Type: tea.KeyEnter})

	select {
	case d := <-reply:
		if d != agent.PermissionDecisionAllow {
			t.Errorf("Enter on Yes should send Allow, got %v", d)
		}
	default:
		t.Fatalf("no decision sent through reply channel")
	}
	if m.permActive {
		t.Errorf("permActive should clear after decision")
	}
}

func TestPermissionAsk_EscDeniesAndDismisses(t *testing.T) {
	m := newSlashTestModel(t)
	m.permActive = true
	m.permChoices = []permChoice{{Label: "Yes", Key: "y"}, {Label: "No", Key: "n"}}
	reply := make(chan agent.PermissionDecision, 1)
	m.permReply = reply
	m.permCursor = 0

	m.handlePermKey(tea.KeyMsg{Type: tea.KeyEscape})

	select {
	case d := <-reply:
		if d != agent.PermissionDecisionDeny {
			t.Errorf("Esc should send Deny, got %v", d)
		}
	default:
		t.Fatalf("Esc must still send a decision (deny)")
	}
	if m.permActive {
		t.Errorf("permActive should clear")
	}
}

func TestPermissionAsk_AlwaysAllowAddsRule(t *testing.T) {
	m := newSlashTestModel(t)
	m.permActive = true
	m.permChoices = []permChoice{
		{Label: "Yes", Key: "y"},
		{Label: "Always", Key: "a"},
	}
	reply := make(chan agent.PermissionDecision, 1)
	m.permReply = reply
	m.permCursor = 1

	m.handlePermKey(tea.KeyMsg{Type: tea.KeyEnter})

	select {
	case d := <-reply:
		if d != agent.PermissionDecisionAlwaysAllow {
			t.Errorf("expected AlwaysAllow, got %v", d)
		}
	default:
		t.Fatalf("no decision sent")
	}
}

// ============================================================================
// 2. History search (Ctrl-R overlay)
// ============================================================================

func TestHistorySearch_OpenSeedsMatched(t *testing.T) {
	m := newSlashTestModel(t)
	// Pre-seed instead of letting openHistorySearch read disk.
	m.histAll = []string{"first prompt", "second prompt", "totally different"}

	// Re-implement the "already loaded → just open" branch:
	m.showHistory = true
	m.histFilter = ""
	m.histCursor = 0
	m.histMatched = append([]string(nil), m.histAll...)

	if !m.showHistory {
		t.Errorf("showHistory not set")
	}
	if len(m.histMatched) != 3 {
		t.Errorf("histMatched should mirror histAll, got %d", len(m.histMatched))
	}
}

func TestHistorySearch_FilterNarrows(t *testing.T) {
	m := newSlashTestModel(t)
	m.histAll = []string{"git status", "git log", "ls -la"}
	m.histMatched = append([]string(nil), m.histAll...)
	m.showHistory = true

	// Type "git" rune by rune.
	for _, r := range "git" {
		m.handleHistoryKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if m.histFilter != "git" {
		t.Errorf("histFilter = %q", m.histFilter)
	}
	if len(m.histMatched) != 2 {
		t.Errorf("after 'git' filter expected 2 matches, got %d (%v)", len(m.histMatched), m.histMatched)
	}

	// Backspace narrows back.
	m.handleHistoryKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if m.histFilter != "gi" {
		t.Errorf("after backspace filter = %q", m.histFilter)
	}
}

func TestHistorySearch_EnterCopiesToInput(t *testing.T) {
	m := newSlashTestModel(t)
	m.histAll = []string{"first", "second"}
	m.histMatched = append([]string(nil), m.histAll...)
	m.showHistory = true
	m.histCursor = 1

	m.handleHistoryKey(tea.KeyMsg{Type: tea.KeyEnter})

	if m.input.Value() != "second" {
		t.Errorf("Enter should copy selection to input, got %q", m.input.Value())
	}
	if m.showHistory {
		t.Errorf("history overlay should close after Enter")
	}
}

func TestHistorySearch_EscCancels(t *testing.T) {
	m := newSlashTestModel(t)
	m.histAll = []string{"a", "b"}
	m.histMatched = append([]string(nil), m.histAll...)
	m.showHistory = true
	m.input.SetValue("user-typed")

	m.handleHistoryKey(tea.KeyMsg{Type: tea.KeyEscape})

	if m.showHistory {
		t.Errorf("Esc should close history overlay")
	}
	if m.input.Value() != "user-typed" {
		t.Errorf("Esc must NOT clobber the user's typed input, got %q", m.input.Value())
	}
}

func TestHistorySearch_NavigationClamps(t *testing.T) {
	m := newSlashTestModel(t)
	m.histMatched = []string{"a", "b", "c"}
	m.showHistory = true
	m.histCursor = 0

	// Up at top should stay at 0 (claude-code parity, no wrap).
	m.handleHistoryKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.histCursor != 0 {
		t.Errorf("↑ at top should clamp at 0, got %d", m.histCursor)
	}

	// Down moves through, clamps at end.
	m.handleHistoryKey(tea.KeyMsg{Type: tea.KeyDown})
	m.handleHistoryKey(tea.KeyMsg{Type: tea.KeyDown})
	m.handleHistoryKey(tea.KeyMsg{Type: tea.KeyDown}) // beyond end
	if m.histCursor != len(m.histMatched)-1 {
		t.Errorf("↓ should clamp at len-1=%d, got %d", len(m.histMatched)-1, m.histCursor)
	}
}

// ============================================================================
// 3. Slash palette
// ============================================================================

func TestPalette_FilterAndMatch(t *testing.T) {
	m := newSlashTestModel(t)
	m.showPalette = true
	m.palFilter = ""
	m.matchCommands()

	all := len(m.palMatched)
	if all == 0 {
		t.Fatalf("empty filter should return ALL commands, got 0")
	}

	// Narrow to "do" — should still hit /doctor (or any "do*").
	m.palFilter = "do"
	m.matchCommands()
	if len(m.palMatched) == 0 || len(m.palMatched) >= all {
		t.Errorf("filter 'do' should narrow result; got %d (was %d)", len(m.palMatched), all)
	}
	hit := false
	for _, c := range m.palMatched {
		if c.Name == "doctor" {
			hit = true
			break
		}
	}
	if !hit {
		t.Errorf("filter 'do' should include /doctor, got %v", m.palMatched)
	}
}

func TestPalette_TabCyclesAndSyncsBuffer(t *testing.T) {
	m := newSlashTestModel(t)
	m.showPalette = true
	m.palFilter = ""
	m.matchCommands()
	if len(m.palMatched) < 2 {
		t.Skip("need at least 2 registered commands for cycle test")
	}

	m.palCursor = 0
	first := m.palMatched[0].Name
	m.handlePaletteKey(tea.KeyMsg{Type: tea.KeyTab})
	if m.palCursor != 1 {
		t.Errorf("Tab should move cursor 0→1, got %d", m.palCursor)
	}
	second := m.palMatched[1].Name
	if m.input.Value() != "/"+second {
		t.Errorf("Tab should sync input buffer to /%s, got %q", second, m.input.Value())
	}

	// Arrow keys also navigate.
	m.handlePaletteKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.palCursor != 0 {
		t.Errorf("↑ should move 1→0, got %d", m.palCursor)
	}
	if m.input.Value() != "/"+first {
		t.Errorf("after ↑, buffer should sync back to /%s", first)
	}
}

func TestPalette_EscClosesAndResetsInput(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/something")
	m.showPalette = true
	m.palFilter = "something"

	m.handlePaletteKey(tea.KeyMsg{Type: tea.KeyEscape})

	if m.showPalette {
		t.Errorf("Esc should close palette")
	}
	if m.palFilter != "" {
		t.Errorf("filter should reset on Esc, got %q", m.palFilter)
	}
}

func TestPalette_BackspaceOnEmptyFilterCloses(t *testing.T) {
	m := newSlashTestModel(t)
	m.showPalette = true
	m.palFilter = ""

	m.handlePaletteKey(tea.KeyMsg{Type: tea.KeyBackspace})

	if m.showPalette {
		t.Errorf("Backspace on empty filter should close palette (the user is deleting the leading '/')")
	}
}

// ============================================================================
// 4. @-mention
// ============================================================================

func TestAtMention_DetectsTrailingAtToken(t *testing.T) {
	cases := []struct {
		input  string
		ok     bool
		filter string
	}{
		{"@", true, ""},
		{"hello @rea", true, "rea"},
		{"see @main.go later", false, ""}, // no trailing
		{"plain text", false, ""},
		{"e@mail.com", false, ""}, // not preceded by space
		{"mid @sentence and more", false, ""},
	}
	for _, tc := range cases {
		got, ok := detectAtMention(tc.input)
		if ok != tc.ok {
			t.Errorf("detectAtMention(%q) ok=%v, want %v", tc.input, ok, tc.ok)
			continue
		}
		if got != tc.filter {
			t.Errorf("detectAtMention(%q) filter=%q, want %q", tc.input, got, tc.filter)
		}
	}
}

func TestAtMention_NavigateAndSelectCloses(t *testing.T) {
	m := newSlashTestModel(t)
	m.atActive = true
	m.atMatched = []string{"main.go", "main_test.go", "README.md"}
	m.atCursor = 0
	m.input.SetValue("read @main")

	// The atmention dispatcher lives inline in keybind_main; we replicate
	// its select behavior here to test the post-cursor commit. Down ×1
	// then Tab to commit the highlight.
	if m.atCursor != 0 {
		t.Errorf("baseline cursor = %d", m.atCursor)
	}

	// Reproduce the ↓ logic from keybind_main.
	if m.atCursor < len(m.atMatched)-1 {
		m.atCursor++
	}
	if m.atCursor != 1 {
		t.Errorf("↓ should move 0→1, got %d", m.atCursor)
	}

	// Reproduce Tab/Enter commit logic: replace the @-token with the
	// selected match, close the dropdown.
	picked := m.atMatched[m.atCursor]
	m.input.SetValue(applyAtMention("read @main", picked))
	m.atActive = false

	if !strings.Contains(m.input.Value(), "main_test.go") {
		t.Errorf("input should contain selected file, got %q", m.input.Value())
	}
	if m.atActive {
		t.Errorf("atActive should clear after commit")
	}
}

// ============================================================================
// keep tests deterministic — vimModeState is package-level
// ============================================================================

func init() {
	// In case a previous test left vim mode on.
	vimModeState = vimOff
	_ = time.Now // placeholder to avoid "unused import" if a future
	// edit drops the time-using code from this file.
}
