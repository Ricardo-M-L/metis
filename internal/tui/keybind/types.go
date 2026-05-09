package keybind

// types.go — pure-data shapes for chord-aware keybindings. The TUI
// (bubbletea) feeds tea.KeyMsg → Keystroke at the integration layer;
// this package stays framework-neutral so it can be unit-tested
// without standing up a tea.Model.
//
// Mirrors claude-code-sourcemap restored-src/src/keybindings/types.ts
// shape. Differences from the JS reference:
//
//   - Go has no union type for ResolveResult — we use a discriminated
//     struct with Type + payload fields. Same expressiveness, more
//     explicit at call sites (no `if (result.type === 'match')` runtime
//     checks; switch on result.Type covers every case).
//   - We collapse `alt` and `meta` to a single `Alt` flag because legacy
//     terminals can't distinguish them anyway (matches claude-code's
//     keystrokesEqual collapsing). `Super` (cmd/win key, only via Kitty
//     keyboard protocol) stays separate.

// Keystroke is one normalized key event. The TUI bridge layer
// translates tea.KeyMsg → Keystroke before feeding the resolver.
//
// Key holds the canonical name lowercase ("a", "f1", "tab", "enter",
// "escape"). Special keys take their stdlib name; printable ASCII is
// just the literal lowercase character.
type Keystroke struct {
	Key   string
	Ctrl  bool
	Alt   bool // also captures Meta — legacy terminals can't tell them apart
	Shift bool
	Super bool // cmd/win — only Kitty protocol surfaces this
}

// Equal reports whether two keystrokes match. Used by the resolver to
// compare a live keystroke against each binding's chord steps.
//
// We DELIBERATELY ignore Shift on letter keys because terminal keyboards
// upper-case the input itself ("A" with shift=false vs "A" with
// shift=true both arrive depending on the terminal). The user's binding
// "shift+a" should match either. This matches claude-code's behaviour.
func (k Keystroke) Equal(o Keystroke) bool {
	if k.Key != o.Key {
		return false
	}
	if k.Ctrl != o.Ctrl {
		return false
	}
	if k.Alt != o.Alt {
		return false
	}
	if k.Super != o.Super {
		return false
	}
	// Shift compared only for non-letter keys — see comment above.
	if !isLetter(k.Key) && k.Shift != o.Shift {
		return false
	}
	return true
}

// isLetter reports whether key is a single ASCII letter where shift
// is implicit in the upper-case form.
func isLetter(s string) bool {
	if len(s) != 1 {
		return false
	}
	c := s[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// Binding is one user-defined or default keybinding. Chord is the
// sequence of keystrokes (length 1 for single-key, ≥2 for chords).
// Action is what to fire when matched; nil means "explicitly unbound"
// (lets a user override a default to a no-op without removing it).
type Binding struct {
	Chord   []Keystroke
	Action  string // empty string ≡ unbound
	Context string // e.g. "Chat", "Global" — only fires when active
}

// IsUnbound reports whether this binding is an explicit "do nothing"
// override. Distinct from "no binding exists" (the resolver returns
// ResolveNone for that).
func (b Binding) IsUnbound() bool { return b.Action == "" }

// ResolveResult is what the (non-chord) resolver returns. Used by
// the simpler single-keystroke API resolveKey; chord-aware paths use
// ChordResolveResult.
type ResolveResult struct {
	Type   ResolveResultType
	Action string // populated only for ResolveMatch
}

type ResolveResultType int

const (
	// ResolveNone — no binding exists for this keystroke.
	ResolveNone ResolveResultType = iota
	// ResolveMatch — exactly one binding fired; Action is set.
	ResolveMatch
	// ResolveUnbound — binding existed but was explicitly bound to nil.
	// The TUI should NOT propagate the keystroke onward.
	ResolveUnbound
)

// ChordResolveResult is what resolveKeyWithChordState returns. Adds
// the two transitional states for multi-key chords.
type ChordResolveResult struct {
	Type    ChordResolveType
	Action  string      // for ChordMatch
	Pending []Keystroke // for ChordStarted — what's been consumed so far
}

type ChordResolveType int

const (
	// ChordNone — no binding, not in a chord.
	ChordNone ChordResolveType = iota
	// ChordMatch — exact match; Action fires.
	ChordMatch
	// ChordUnbound — match but explicitly unbound.
	ChordUnbound
	// ChordStarted — keystroke is a prefix of one or more chord
	// bindings; the resolver wants the next keystroke. Pending is the
	// state to feed into the next call.
	ChordStarted
	// ChordCancelled — was in a chord (Pending != nil) but the next
	// keystroke didn't match any continuation. The TUI should clear
	// the pending state and (typically) pass the keystroke through to
	// the inner widget.
	ChordCancelled
)
