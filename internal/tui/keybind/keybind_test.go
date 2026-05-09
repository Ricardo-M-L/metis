package keybind

import (
	"runtime"
	"testing"
)

// ─── Parser ──────────────────────────────────────────────────────

func TestParseChord_SingleKey(t *testing.T) {
	got, err := ParseChord("ctrl+s")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 step, got %d", len(got))
	}
	want := Keystroke{Key: "s", Ctrl: true}
	if !got[0].Equal(want) {
		t.Errorf("step: got %+v, want %+v", got[0], want)
	}
}

func TestParseChord_MultiStep(t *testing.T) {
	got, err := ParseChord("ctrl+k ctrl+s")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(got))
	}
	if got[0].Key != "k" || !got[0].Ctrl {
		t.Errorf("step 0: %+v", got[0])
	}
	if got[1].Key != "s" || !got[1].Ctrl {
		t.Errorf("step 1: %+v", got[1])
	}
}

func TestParseChord_ModifierAliases(t *testing.T) {
	cases := []string{"ctrl+a", "control+a", "Control+a", "CTRL+a"}
	for _, c := range cases {
		got, err := ParseChord(c)
		if err != nil {
			t.Errorf("%q: %v", c, err)
			continue
		}
		if !got[0].Ctrl || got[0].Key != "a" {
			t.Errorf("%q parsed wrong: %+v", c, got[0])
		}
	}
}

func TestParseChord_AltAndMetaAreSame(t *testing.T) {
	a, _ := ParseChord("alt+enter")
	b, _ := ParseChord("opt+enter")
	c, _ := ParseChord("meta+enter")
	if !a[0].Equal(b[0]) || !b[0].Equal(c[0]) {
		t.Error("alt/opt/meta should all collapse to Alt=true")
	}
	if !a[0].Alt {
		t.Error("Alt flag missing")
	}
}

func TestParseChord_CmdMapsToSuper(t *testing.T) {
	got, _ := ParseChord("cmd+c")
	if !got[0].Super || got[0].Key != "c" {
		t.Errorf("cmd+c: %+v", got[0])
	}
}

func TestParseChord_KeyAliases(t *testing.T) {
	cases := []struct{ in, key string }{
		{"return", "enter"},
		{"esc", "escape"},
		{"del", "delete"},
		{"pgup", "pageup"},
		{"pgdn", "pagedown"},
		{"space", "space"},
	}
	for _, c := range cases {
		got, err := ParseChord(c.in)
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Key != c.key {
			t.Errorf("%q → %q, want %q", c.in, got[0].Key, c.key)
		}
	}
}

func TestParseChord_RejectsEmpty(t *testing.T) {
	if _, err := ParseChord(""); err == nil {
		t.Error("empty chord must error")
	}
	if _, err := ParseChord("   "); err == nil {
		t.Error("whitespace-only chord must error")
	}
}

func TestParseChord_RejectsMissingKey(t *testing.T) {
	if _, err := ParseChord("ctrl+"); err == nil {
		t.Error("missing main key must error")
	}
	if _, err := ParseChord("ctrl"); err == nil {
		t.Error("modifier-only must error")
	}
}

func TestParseChord_RejectsTwoMainKeys(t *testing.T) {
	if _, err := ParseChord("a+b"); err == nil {
		t.Error("two main keys must error")
	}
}

// ─── Format round-trip ──────────────────────────────────────────

func TestFormatKeystroke_RoundTrip(t *testing.T) {
	cases := []string{
		"a", "ctrl+s", "alt+enter", "ctrl+shift+f1",
		"super+space", "f12", "tab",
	}
	for _, c := range cases {
		parsed, err := ParseChord(c)
		if err != nil {
			t.Fatal(err)
		}
		out := FormatKeystroke(parsed[0])
		// Re-parse and compare structurally — exact string round-trip
		// isn't required (modifier order / case may normalise).
		reparsed, err := ParseChord(out)
		if err != nil {
			t.Errorf("re-parse of %q failed: %v", out, err)
			continue
		}
		if !parsed[0].Equal(reparsed[0]) {
			t.Errorf("%q → %q → %+v ≠ %+v", c, out, reparsed[0], parsed[0])
		}
	}
}

func TestFormatChord_MultiStep(t *testing.T) {
	parsed, _ := ParseChord("ctrl+k ctrl+s")
	out := FormatChord(parsed)
	if out != "ctrl+k ctrl+s" {
		t.Errorf("format: got %q, want 'ctrl+k ctrl+s'", out)
	}
}

func TestFormatKeystroke_SkipsShiftOnLetters(t *testing.T) {
	// shift+a and a should format the same since shift is implicit
	// in upper-case for letter keys.
	withShift := Keystroke{Key: "a", Shift: true}
	withoutShift := Keystroke{Key: "a"}
	if FormatKeystroke(withShift) != FormatKeystroke(withoutShift) {
		t.Errorf("shift+a should format identical to a, got %q vs %q",
			FormatKeystroke(withShift), FormatKeystroke(withoutShift))
	}
}

// ─── Keystroke.Equal ──────────────────────────────────────────────

func TestKeystrokeEqual_Basic(t *testing.T) {
	a := Keystroke{Key: "s", Ctrl: true}
	b := Keystroke{Key: "s", Ctrl: true}
	if !a.Equal(b) {
		t.Error("identical keystrokes must be equal")
	}
}

func TestKeystrokeEqual_DifferentKeyRejects(t *testing.T) {
	a := Keystroke{Key: "s", Ctrl: true}
	b := Keystroke{Key: "x", Ctrl: true}
	if a.Equal(b) {
		t.Error("different keys must not be equal")
	}
}

func TestKeystrokeEqual_ShiftOnLetterIgnored(t *testing.T) {
	// shift on letter should be ignored in equality check.
	a := Keystroke{Key: "a", Shift: false}
	b := Keystroke{Key: "a", Shift: true}
	if !a.Equal(b) {
		t.Error("shift on letter key should be ignored by Equal")
	}
}

func TestKeystrokeEqual_ShiftOnNonLetterChecked(t *testing.T) {
	// shift+f1 vs f1 should differ — shift on non-letter is significant.
	a := Keystroke{Key: "f1", Shift: false}
	b := Keystroke{Key: "f1", Shift: true}
	if a.Equal(b) {
		t.Error("shift on f1 should be significant in Equal")
	}
}

// ─── ResolveKey (single-keystroke) ──────────────────────────────

func TestResolveKey_Match(t *testing.T) {
	chord, _ := ParseChord("ctrl+s")
	bindings := []Binding{
		{Chord: chord, Action: "save", Context: "Chat"},
	}
	res := ResolveKey(chord[0], []string{"Chat"}, bindings)
	if res.Type != ResolveMatch {
		t.Fatalf("got %v, want ResolveMatch", res.Type)
	}
	if res.Action != "save" {
		t.Errorf("action: %q", res.Action)
	}
}

func TestResolveKey_NoMatchReturnsNone(t *testing.T) {
	chord, _ := ParseChord("ctrl+s")
	bindings := []Binding{
		{Chord: chord, Action: "save", Context: "Chat"},
	}
	other, _ := ParseChord("ctrl+x")
	res := ResolveKey(other[0], []string{"Chat"}, bindings)
	if res.Type != ResolveNone {
		t.Errorf("got %v, want ResolveNone", res.Type)
	}
}

func TestResolveKey_ContextFilter(t *testing.T) {
	chord, _ := ParseChord("ctrl+s")
	bindings := []Binding{
		{Chord: chord, Action: "save", Context: "Editor"},
	}
	res := ResolveKey(chord[0], []string{"Chat"}, bindings)
	if res.Type != ResolveNone {
		t.Errorf("Editor binding shouldn't fire in Chat context, got %v", res.Type)
	}
}

func TestResolveKey_LastBindingWins(t *testing.T) {
	chord, _ := ParseChord("ctrl+s")
	bindings := []Binding{
		{Chord: chord, Action: "default-save", Context: "Chat"},
		{Chord: chord, Action: "user-override", Context: "Chat"},
	}
	res := ResolveKey(chord[0], []string{"Chat"}, bindings)
	if res.Action != "user-override" {
		t.Errorf("user override should win, got %q", res.Action)
	}
}

func TestResolveKey_UnboundDistinctFromNone(t *testing.T) {
	chord, _ := ParseChord("ctrl+s")
	bindings := []Binding{
		{Chord: chord, Action: "save", Context: "Chat"},
		{Chord: chord, Action: "", Context: "Chat"}, // user explicit unbind
	}
	res := ResolveKey(chord[0], []string{"Chat"}, bindings)
	if res.Type != ResolveUnbound {
		t.Errorf("explicit empty action should be ResolveUnbound, got %v", res.Type)
	}
}

// ─── Chord state machine ─────────────────────────────────────────

func TestChordResolver_PrefixStartsChord(t *testing.T) {
	chord, _ := ParseChord("ctrl+k ctrl+s")
	bindings := []Binding{
		{Chord: chord, Action: "stash", Context: "Chat"},
	}
	first, _ := ParseChord("ctrl+k")
	res := ResolveKeyWithChordState(first[0], []string{"Chat"}, bindings, nil)
	if res.Type != ChordStarted {
		t.Fatalf("ctrl+k should start chord, got %v", res.Type)
	}
	if len(res.Pending) != 1 {
		t.Errorf("Pending: got %d, want 1", len(res.Pending))
	}
}

func TestChordResolver_ContinuationMatches(t *testing.T) {
	chord, _ := ParseChord("ctrl+k ctrl+s")
	bindings := []Binding{
		{Chord: chord, Action: "stash", Context: "Chat"},
	}
	first, _ := ParseChord("ctrl+k")
	pending := []Keystroke{first[0]}
	second, _ := ParseChord("ctrl+s")
	res := ResolveKeyWithChordState(second[0], []string{"Chat"}, bindings, pending)
	if res.Type != ChordMatch {
		t.Fatalf("got %v, want ChordMatch", res.Type)
	}
	if res.Action != "stash" {
		t.Errorf("action: %q", res.Action)
	}
}

func TestChordResolver_WrongContinuationCancels(t *testing.T) {
	chord, _ := ParseChord("ctrl+k ctrl+s")
	bindings := []Binding{
		{Chord: chord, Action: "stash", Context: "Chat"},
	}
	first, _ := ParseChord("ctrl+k")
	pending := []Keystroke{first[0]}
	wrong, _ := ParseChord("ctrl+x")
	res := ResolveKeyWithChordState(wrong[0], []string{"Chat"}, bindings, pending)
	if res.Type != ChordCancelled {
		t.Errorf("wrong continuation should cancel, got %v", res.Type)
	}
}

func TestChordResolver_EscapeCancelsMidChord(t *testing.T) {
	chord, _ := ParseChord("ctrl+k ctrl+s")
	bindings := []Binding{
		{Chord: chord, Action: "stash", Context: "Chat"},
	}
	first, _ := ParseChord("ctrl+k")
	pending := []Keystroke{first[0]}
	esc := Keystroke{Key: "escape"}
	res := ResolveKeyWithChordState(esc, []string{"Chat"}, bindings, pending)
	if res.Type != ChordCancelled {
		t.Errorf("Esc should cancel chord, got %v", res.Type)
	}
}

func TestChordResolver_SingleKeyStillWorksMidEverything(t *testing.T) {
	// ctrl+s should still match its own single-key binding even when
	// other chords starting with different keys exist.
	cs, _ := ParseChord("ctrl+s")
	ckcs, _ := ParseChord("ctrl+k ctrl+s")
	bindings := []Binding{
		{Chord: cs, Action: "save", Context: "Chat"},
		{Chord: ckcs, Action: "stash", Context: "Chat"},
	}
	res := ResolveKeyWithChordState(cs[0], []string{"Chat"}, bindings, nil)
	if res.Type != ChordMatch || res.Action != "save" {
		t.Errorf("ctrl+s should match save, got %+v", res)
	}
}

func TestChordResolver_UnboundDoesntCaptureChord(t *testing.T) {
	// User unbinds the default `ctrl+x ctrl+k`. Now `ctrl+x` alone
	// should NOT start a chord — we'd be stuck waiting for ctrl+k
	// that never fires anything.
	cxck, _ := ParseChord("ctrl+x ctrl+k")
	cs, _ := ParseChord("ctrl+s")
	bindings := []Binding{
		{Chord: cxck, Action: "default-stash", Context: "Chat"}, // default
		{Chord: cxck, Action: "", Context: "Chat"},              // user unbinds
		{Chord: cs, Action: "save", Context: "Chat"},
	}
	cx, _ := ParseChord("ctrl+x")
	res := ResolveKeyWithChordState(cx[0], []string{"Chat"}, bindings, nil)
	if res.Type == ChordStarted {
		t.Errorf("unbound chord should NOT capture single-key prefix, got %+v", res)
	}
}

func TestChordResolver_NoBindingReturnsNone(t *testing.T) {
	res := ResolveKeyWithChordState(
		Keystroke{Key: "s", Ctrl: true},
		[]string{"Chat"},
		nil, nil,
	)
	if res.Type != ChordNone {
		t.Errorf("no bindings → ChordNone, got %v", res.Type)
	}
}

// ─── Reserved shortcuts ──────────────────────────────────────────

func TestNonRebindable_HasCriticalKeys(t *testing.T) {
	want := []string{"ctrl+c", "ctrl+d", "ctrl+m"}
	got := map[string]bool{}
	for _, r := range NonRebindable {
		got[r.Key] = true
		if r.Severity != SeverityError {
			t.Errorf("%s should be severity error, got %v", r.Key, r.Severity)
		}
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing critical reserved key: %s", w)
		}
	}
}

func TestCheckReserved_DetectsCtrlC(t *testing.T) {
	r, ok := CheckReserved("ctrl+c")
	if !ok {
		t.Fatal("ctrl+c should be reported as reserved")
	}
	if r.Severity != SeverityError {
		t.Errorf("severity: %v", r.Severity)
	}
	if r.Key != "ctrl+c" {
		t.Errorf("key: %q", r.Key)
	}
}

func TestCheckReserved_NormalisesCase(t *testing.T) {
	cases := []string{"ctrl+c", "Ctrl+C", "CTRL+C", "control+c"}
	for _, c := range cases {
		if _, ok := CheckReserved(c); !ok {
			t.Errorf("%q should be detected as reserved", c)
		}
	}
}

func TestCheckReserved_ChordWithReservedFirstStepFlagged(t *testing.T) {
	// A chord starting with ctrl+c is broken even though ctrl+c is
	// the first step — the chord can never fire because ctrl+c is
	// always intercepted.
	if _, ok := CheckReserved("ctrl+c ctrl+x"); !ok {
		t.Error("chord starting with ctrl+c should be flagged")
	}
}

func TestCheckReserved_SafeKeyNotReserved(t *testing.T) {
	if _, ok := CheckReserved("ctrl+t"); ok {
		t.Error("ctrl+t is not reserved")
	}
	if _, ok := CheckReserved("alt+enter"); ok {
		t.Error("alt+enter is not reserved")
	}
}

func TestCheckReserved_QuitSignalIsError(t *testing.T) {
	r, ok := CheckReserved("ctrl+\\")
	if !ok {
		t.Fatal("ctrl+\\ should be reserved")
	}
	if r.Severity != SeverityError {
		t.Errorf("ctrl+\\ should be error severity, got %v", r.Severity)
	}
}

func TestAllReserved_PlatformAware(t *testing.T) {
	got := AllReserved()
	hasMacOS := false
	for _, r := range got {
		if r.Key == "cmd+c" {
			hasMacOS = true
		}
	}
	if runtime.GOOS == "darwin" {
		if !hasMacOS {
			t.Error("on darwin, AllReserved should include cmd+c")
		}
	} else {
		if hasMacOS {
			t.Error("off darwin, AllReserved should NOT include macOS shortcuts")
		}
	}
}

func TestSeverityString(t *testing.T) {
	if SeverityError.String() != "error" {
		t.Errorf("SeverityError.String() = %q", SeverityError.String())
	}
	if SeverityWarning.String() != "warning" {
		t.Errorf("SeverityWarning.String() = %q", SeverityWarning.String())
	}
}
