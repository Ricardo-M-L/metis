package keybind

// resolver.go — pure functions that turn a keystroke + the current
// chord state into a ResolveResult. No mutable state, no I/O — the
// TUI bridge holds the pending []Keystroke between calls.
//
// Mirrors claude-code-sourcemap restored-src/src/keybindings/resolver.ts
// resolveKeyWithChordState. The state machine:
//
//   ┌──────────┐  prefix  ┌───────────────┐  next-keystroke
//   │  None    │ ───────▶ │ ChordStarted  │ ────────────────▶ Match / Cancelled
//   │ pending=∅│          │ pending=[…]   │
//   └──────────┘ ◀──────── └───────────────┘
//                  match    no-match (Cancelled clears pending)

// ResolveKey is the simple single-keystroke resolver. Call sites that
// don't need chord support use this — saves a tiny bit of work and
// makes the failure modes obvious.
func ResolveKey(k Keystroke, activeContexts []string, bindings []Binding) ResolveResult {
	ctxSet := setOf(activeContexts)
	var match *Binding
	for i := range bindings {
		b := &bindings[i]
		if len(b.Chord) != 1 {
			continue
		}
		if !ctxSet[b.Context] {
			continue
		}
		if b.Chord[0].Equal(k) {
			match = b // last one wins for user overrides
		}
	}
	if match == nil {
		return ResolveResult{Type: ResolveNone}
	}
	if match.IsUnbound() {
		return ResolveResult{Type: ResolveUnbound}
	}
	return ResolveResult{Type: ResolveMatch, Action: match.Action}
}

// ResolveKeyWithChordState is the chord-aware resolver. Maintains
// state via the pending parameter — caller threads it through:
//
//	state.pending = nil
//	for each keystroke {
//	    res := ResolveKeyWithChordState(k, contexts, bindings, state.pending)
//	    switch res.Type {
//	    case ChordStarted:    state.pending = res.Pending
//	    case ChordMatch:      state.pending = nil; fire(res.Action)
//	    case ChordCancelled:  state.pending = nil; passthrough(k)
//	    case ChordNone:       passthrough(k)
//	    case ChordUnbound:    state.pending = nil  // swallow, no fire
//	    }
//	}
//
// Special: a bare Escape while pending != nil cancels the chord
// without consuming the Escape (so Esc still works for its normal
// "abort current input" purpose).
func ResolveKeyWithChordState(k Keystroke, activeContexts []string, bindings []Binding, pending []Keystroke) ChordResolveResult {
	// Bare Escape while in a chord cancels.
	if k.Key == "escape" && !k.Ctrl && !k.Alt && !k.Super && len(pending) > 0 {
		return ChordResolveResult{Type: ChordCancelled}
	}

	testChord := append(append([]Keystroke{}, pending...), k)
	ctxSet := setOf(activeContexts)

	// Find any binding whose chord this testChord is a strict prefix of.
	// Group by chord-string: a later null override unbinds the default,
	// so null-action prefix matches don't keep us in chord_started state
	// (otherwise unbinding `ctrl+x ctrl+k` would still capture `ctrl+x`).
	chordWinners := map[string]string{}
	hasNonEmptyChord := false
	for _, b := range bindings {
		if !ctxSet[b.Context] {
			continue
		}
		if len(b.Chord) <= len(testChord) {
			continue
		}
		if !chordPrefixMatches(testChord, b.Chord) {
			continue
		}
		key := FormatChord(b.Chord)
		if _, seen := chordWinners[key]; seen {
			// last-write-wins — let user overrides shadow defaults.
		}
		chordWinners[key] = b.Action
	}
	for _, action := range chordWinners {
		if action != "" {
			hasNonEmptyChord = true
			break
		}
	}

	if hasNonEmptyChord {
		return ChordResolveResult{Type: ChordStarted, Pending: testChord}
	}

	// No prefix → look for an exact match of the full testChord.
	var exact *Binding
	for i := range bindings {
		b := &bindings[i]
		if !ctxSet[b.Context] {
			continue
		}
		if chordExactlyMatches(testChord, b.Chord) {
			exact = b
		}
	}
	if exact != nil {
		if exact.IsUnbound() {
			return ChordResolveResult{Type: ChordUnbound}
		}
		return ChordResolveResult{Type: ChordMatch, Action: exact.Action}
	}

	// No match. If we WERE in a chord, the chord just got cancelled
	// (next keystroke didn't continue). Caller should clear pending
	// and pass the keystroke through to the inner widget.
	if len(pending) > 0 {
		return ChordResolveResult{Type: ChordCancelled}
	}
	return ChordResolveResult{Type: ChordNone}
}

// chordPrefixMatches reports whether prefix is a strict prefix of
// chord (i.e. shorter than chord AND every position matches).
func chordPrefixMatches(prefix, chord []Keystroke) bool {
	if len(prefix) >= len(chord) {
		return false
	}
	for i, p := range prefix {
		if !p.Equal(chord[i]) {
			return false
		}
	}
	return true
}

// chordExactlyMatches reports whether two chords are equal length
// and every position matches.
func chordExactlyMatches(a, b []Keystroke) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

// setOf turns []string into a lookup map. Tiny helper, inlined to
// avoid a generics-import dance.
func setOf(s []string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}
