package keybind

// parser.go — turn human-readable binding strings into []Keystroke
// chords. Examples:
//
//	"ctrl+s"         → [{Key:"s",Ctrl:true}]
//	"ctrl+k ctrl+s"  → [{Key:"k",Ctrl:true},{Key:"s",Ctrl:true}]
//	"alt+enter"      → [{Key:"enter",Alt:true}]
//	"shift+f1"       → [{Key:"f1",Shift:true}]
//
// Steps separated by whitespace are chord steps. Within a step,
// modifiers and the main key are joined with `+`. Modifier names are
// case-insensitive and several aliases are recognised (control/ctrl,
// option/opt/alt, command/cmd/meta).

import (
	"fmt"
	"sort"
	"strings"
)

// ParseChord turns a binding string into the keystroke sequence it
// represents. Returns an error on:
//
//   - empty step (two spaces in a row, or trailing space)
//   - unknown modifier ("super-duper")
//   - missing main key (just "ctrl+")
//   - duplicate main keys in one step ("a+b")
func ParseChord(s string) ([]Keystroke, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("keybind: empty chord")
	}
	steps := strings.Fields(s)
	out := make([]Keystroke, 0, len(steps))
	for _, step := range steps {
		k, err := parseStep(step)
		if err != nil {
			return nil, fmt.Errorf("keybind: parse %q: %w", s, err)
		}
		out = append(out, k)
	}
	return out, nil
}

func parseStep(step string) (Keystroke, error) {
	parts := strings.Split(step, "+")
	if len(parts) == 1 && parts[0] == "" {
		return Keystroke{}, fmt.Errorf("empty step")
	}
	var k Keystroke
	mainSet := false
	for _, p := range parts {
		lower := strings.ToLower(strings.TrimSpace(p))
		switch lower {
		case "":
			return Keystroke{}, fmt.Errorf("empty modifier or key in step %q", step)
		case "ctrl", "control":
			k.Ctrl = true
		case "alt", "opt", "option", "meta":
			// claude-code collapses alt/meta because legacy terminals
			// can't distinguish them; we do the same for parsing.
			k.Alt = true
		case "shift":
			k.Shift = true
		case "cmd", "command", "super", "win":
			// "cmd" is the macOS marketing name; in our types it's
			// Super (the kitty-protocol-only modifier). On non-mac
			// platforms a config that uses "cmd" still parses without
			// error — the binding just won't fire.
			k.Super = true
		default:
			if mainSet {
				return Keystroke{}, fmt.Errorf("step %q has two main keys", step)
			}
			k.Key = canonicalKey(lower)
			mainSet = true
		}
	}
	if !mainSet {
		return Keystroke{}, fmt.Errorf("step %q has no main key", step)
	}
	return k, nil
}

// canonicalKey normalises keynames so user-config "Enter" / "RETURN"
// / "enter" all match the runtime keystroke (which arrives as
// "enter" from the TUI bridge). Keep this list short — only common
// aliases. Anything not listed is passed through lowercased.
func canonicalKey(s string) string {
	switch s {
	case "return":
		return "enter"
	case "esc":
		return "escape"
	case "del":
		return "delete"
	case "ins":
		return "insert"
	case "pgup":
		return "pageup"
	case "pgdown", "pgdn":
		return "pagedown"
	case "spc", "space":
		return "space"
	}
	return s
}

// FormatKeystroke is the inverse of parseStep — turns a Keystroke
// back into "ctrl+shift+a" form. Used in `metis keybindings` listing
// + shortcut hint UI.
//
// Modifier order is alphabetical (alt, ctrl, shift, super) so two
// equivalent bindings print identically.
func FormatKeystroke(k Keystroke) string {
	mods := []string{}
	if k.Alt {
		mods = append(mods, "alt")
	}
	if k.Ctrl {
		mods = append(mods, "ctrl")
	}
	if k.Shift && !isLetter(k.Key) {
		// Skip shift on letters since shift is implicit in upper-case.
		mods = append(mods, "shift")
	}
	if k.Super {
		mods = append(mods, "super")
	}
	sort.Strings(mods)
	if k.Key == "" {
		return strings.Join(mods, "+")
	}
	if len(mods) == 0 {
		return k.Key
	}
	return strings.Join(mods, "+") + "+" + k.Key
}

// FormatChord — inverse of ParseChord. Useful for round-trip tests
// + display of resolved bindings ("ctrl+k ctrl+s").
func FormatChord(chord []Keystroke) string {
	parts := make([]string, 0, len(chord))
	for _, k := range chord {
		parts = append(parts, FormatKeystroke(k))
	}
	return strings.Join(parts, " ")
}
