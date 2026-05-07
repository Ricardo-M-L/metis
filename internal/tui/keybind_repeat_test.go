package tui

// keybind_repeat_test.go — covers the "user reports flicker when typing
// the same character repeatedly" report from 2026-05-07 21:18 video:
// pressing 1 over and over made the input field oscillate between "1"
// and "11" instead of growing monotonically.
//
// The unit-test path here drives the model with synthetic KeyPressMsg
// events. If the bug is in event handling (a guard double-fires, the
// textarea state mutates twice, etc.) it'll show up as the buffer not
// reaching the expected length. If the bug is purely in rendering
// (DynamicHeight ping-pong, alt-screen redraw race) this test will
// pass and the issue will only repro under PTY.

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// pressRune is a tiny helper that sends a single printable rune through
// the model's main key handler exactly the way bubbletea would deliver
// it from the terminal.
func pressRune(t *testing.T, m *Model, r rune) {
	t.Helper()
	m.handleKey(tea.KeyPressMsg(tea.Key{Code: r, Text: string(r)}))
}

// TestKeyRepeat_DigitOneAccumulates — pressing "1" five times in a row
// must end with the input value being "11111". The user's video showed
// the value oscillating between "1" and "11" — that would mean either
// the textarea silently dropped keypresses or some upstream guard ate
// every other one.
func TestKeyRepeat_DigitOneAccumulates(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.input.Focus()
	m.histDirectIdx = -1

	for i := 0; i < 5; i++ {
		pressRune(t, m, '1')
		got := m.input.Value()
		want := repeatString("1", i+1)
		if got != want {
			t.Errorf("after %d presses of '1': got %q, want %q", i+1, got, want)
		}
	}
	if got := m.input.Value(); got != "11111" {
		t.Errorf("final value should be %q; got %q", "11111", got)
	}
}

// TestKeyRepeat_MixedCharsAccumulate — same property, but with a mix
// of characters. Exercises the path where every printable rune flows
// through handleKey → updateAtMention → m.input.Update without state
// drift.
func TestKeyRepeat_MixedCharsAccumulate(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.input.Focus()
	m.histDirectIdx = -1

	seq := "abc123"
	for i, r := range seq {
		pressRune(t, m, r)
		got := m.input.Value()
		want := seq[:i+1]
		if got != want {
			t.Errorf("after typing %q: got %q, want %q", seq[:i+1], got, want)
		}
	}
}

// TestKeyRepeat_AfterArrowDoesntCorrupt — types "1" once, presses ↑
// (which now jumps to col 0 on single-line input — see arrow_jump
// tests), then types "2". Expected buffer = "21" (cursor was at col
// 0, so "2" inserted at the start). If the arrow handler's intercept
// somehow re-enters the textarea path it could end up dropping or
// duplicating runes.
func TestKeyRepeat_AfterArrowDoesntCorrupt(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.input.Focus()
	m.histDirectIdx = -1

	pressRune(t, m, '1')
	if got := m.input.Value(); got != "1" {
		t.Fatalf("after '1': got %q, want %q", got, "1")
	}

	pressKey(t, m, "up") // jumps cursor to col 0

	pressRune(t, m, '2')
	if got := m.input.Value(); got != "21" {
		t.Errorf("expected '2' inserted at col 0 → %q; got %q", "21", got)
	}
}

func repeatString(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
