package keybind

// reserved.go — three-tier "shortcuts you can't (or shouldn't) bind"
// list. Mirrors claude-code-sourcemap restored-src/src/keybindings/
// reservedShortcuts.ts categorisation.
//
//   1. NonRebindable   — hardcoded; rebinding is rejected with a
//                        clear error. ctrl+c / ctrl+d / ctrl+m.
//   2. TerminalReserved — terminal/OS intercepts so the keystroke
//                         likely never reaches metis. We emit a
//                         warning if the user binds them anyway.
//   3. MacOSReserved   — macOS-only system shortcuts. Same warning.
//
// The "severity" field lets the IDE / config validator decide how to
// surface the violation: errors block the user's binding, warnings
// just print a one-line note and leave the binding alone (terminal
// flow control might be disabled and the binding actually works).

import "runtime"

// Severity classifies a reserved-shortcut violation.
type Severity int

const (
	// SeverityError — the binding is rejected outright.
	SeverityError Severity = iota
	// SeverityWarning — accepted but flagged; user is told the
	// terminal/OS may swallow the keystroke before metis sees it.
	SeverityWarning
)

func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	}
	return "unknown"
}

// Reserved describes one off-limits shortcut.
type Reserved struct {
	Key      string // canonical form, e.g. "ctrl+c"
	Reason   string
	Severity Severity
}

// NonRebindable — hardcoded in metis. Cannot be overridden, ever.
//
// ctrl+c — universal interrupt. metis catches it for cancel-current-
//
//	turn. Rebinding would brick the only escape hatch from a runaway
//	bash subprocess.
//
// ctrl+d — EOF / quit. Rebinding would prevent clean shell exit
//
//	from the chat REPL.
//
// ctrl+m — terminals send a literal CR for ctrl+m, so it's
//
//	indistinguishable from Enter. Letting users "bind ctrl+m to X"
//	would silently fire X every time they press Enter. Hardcoded
//	error.
var NonRebindable = []Reserved{
	{Key: "ctrl+c", Reason: "Cannot be rebound — used for interrupt/exit (hardcoded)", Severity: SeverityError},
	{Key: "ctrl+d", Reason: "Cannot be rebound — used for EOF/exit (hardcoded)", Severity: SeverityError},
	{Key: "ctrl+m", Reason: "Cannot be rebound — terminals send CR for ctrl+m, identical to Enter", Severity: SeverityError},
}

// TerminalReserved — likely intercepted by the terminal or OS before
// metis ever sees the keystroke. We flag binds against these, but
// don't reject — modern terminals often disable flow control
// (ctrl+s/q) etc., so a user who knows their setup can still use
// them.
//
// ctrl+z — Unix process suspend (SIGTSTP). Some terminals don't
//
//	forward; metis would miss the binding randomly.
//
// ctrl+\ — terminal quit signal (SIGQUIT). Hard error rather than
//
//	warning because metis core panics on SIGQUIT and there's no
//	way to recover the chat state.
//
// ctrl+s / ctrl+q deliberately NOT here — metis uses ctrl+s for
// stash-and-yank and most modern terminals disable flow control by
// default; flagging would create noise for power users.
var TerminalReserved = []Reserved{
	{Key: "ctrl+z", Reason: "Unix process suspend (SIGTSTP) — terminal may intercept", Severity: SeverityWarning},
	{Key: "ctrl+\\", Reason: "Terminal SIGQUIT — metis cannot recover from this", Severity: SeverityError},
}

// MacOSReserved — system shortcuts only the macOS WindowServer can
// see. metis never receives them; binding is harmless but pointless.
// All warnings, not errors.
var MacOSReserved = []Reserved{
	{Key: "cmd+c", Reason: "macOS system copy", Severity: SeverityWarning},
	{Key: "cmd+v", Reason: "macOS system paste", Severity: SeverityWarning},
	{Key: "cmd+x", Reason: "macOS system cut", Severity: SeverityWarning},
	{Key: "cmd+q", Reason: "macOS quit application", Severity: SeverityWarning},
	{Key: "cmd+w", Reason: "macOS close window/tab", Severity: SeverityWarning},
	{Key: "cmd+tab", Reason: "macOS app switcher", Severity: SeverityWarning},
	{Key: "cmd+space", Reason: "macOS Spotlight", Severity: SeverityWarning},
}

// AllReserved returns every reserved shortcut applicable to the
// current platform — NonRebindable + TerminalReserved everywhere,
// plus MacOSReserved on darwin.
func AllReserved() []Reserved {
	out := make([]Reserved, 0, len(NonRebindable)+len(TerminalReserved)+len(MacOSReserved))
	out = append(out, NonRebindable...)
	out = append(out, TerminalReserved...)
	if runtime.GOOS == "darwin" {
		out = append(out, MacOSReserved...)
	}
	return out
}

// CheckReserved looks up a binding string in the platform's reserved
// list. Returns the matching Reserved + true when found, zero +
// false otherwise. Caller decides what to do with errors vs warnings.
//
// Comparison is normalised — "Ctrl+C" matches "ctrl+c" matches
// "control+c". Multi-step chords are checked against the FIRST step
// only (a chord starting with ctrl+c is just as broken as bare
// ctrl+c).
func CheckReserved(bindingStr string) (Reserved, bool) {
	steps, err := ParseChord(bindingStr)
	if err != nil || len(steps) == 0 {
		return Reserved{}, false
	}
	first := FormatKeystroke(steps[0])
	for _, r := range AllReserved() {
		if normalizeBindingKey(r.Key) == first {
			return r, true
		}
	}
	return Reserved{}, false
}

// normalizeBindingKey runs r.Key through the parser to normalise its
// form. Defensive — our static lists already use the canonical form,
// but if a future entry has different casing this stays correct.
func normalizeBindingKey(key string) string {
	steps, err := ParseChord(key)
	if err != nil || len(steps) == 0 {
		return key
	}
	return FormatKeystroke(steps[0])
}
