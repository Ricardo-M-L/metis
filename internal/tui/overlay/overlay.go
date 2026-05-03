// Package overlay defines the contract every modal/dialog/popup follows
// inside the chat surface, plus a Stack that owns z-order + keyboard
// focus routing.
//
// Why this exists: the main bubbletea Model accumulated 5+ overlay
// state-blobs as parallel boolean flags (showPalette / showHistory /
// permActive / btwActive / showTaskPanel / atActive ...). Each new
// modal forced touching keybind dispatch, render, and Esc handling in
// 4-5 places. With Overlay + Stack, a new modal lands in one new file
// implementing this interface plus one Push() call.
//
// Design choices vs claude-code (Ink):
//   - claude-code mounts/unmounts components in JSX; we don't have
//     React lifecycle, so OnPush / OnPop are explicit hooks for any
//     "fire a Cmd when this opens/closes" needs.
//   - claude-code's modals each have their own internal useState; here
//     each Overlay is a struct with its own fields — same outcome,
//     different mechanism.
//   - Stack is intentionally simple (no history, no transitions, no
//     animation). 5–10 overlays is the projected ceiling for metis;
//     YAGNI on richer UX state machines.
package overlay

import (
	tea "charm.land/bubbletea/v2"
)

// Overlay is the contract every modal / dialog / popup implements.
// Methods are designed so the host Model can drive overlays without
// knowing their concrete types.
type Overlay interface {
	// Name returns a short identifier (palette / history / permission /
	// btw / atmention / etc). Used for debug logging + duplicate-push
	// detection.
	Name() string

	// Active reports whether the overlay should currently consume
	// keystrokes + render. An Active=false overlay sitting on the stack
	// is essentially a no-op; it stays for the host's convenience but
	// won't intercept anything. Most overlays will simply track an
	// internal bool and return it here.
	Active() bool

	// Update receives keyboard input *only when this is the top active
	// overlay*. Returns the (possibly mutated) overlay, an optional
	// tea.Cmd, and a flag indicating whether the input was consumed.
	// When Consumed=false the host bubbles the key up to the next layer
	// (the chat surface input box).
	Update(msg tea.KeyMsg) (next Overlay, cmd tea.Cmd, consumed bool)

	// View renders the overlay onto the chat surface. width/height are
	// the terminal dimensions; the overlay decides its own size + lipgloss
	// placement. Empty string when the overlay shouldn't render (e.g.
	// loading state with nothing to show yet).
	View(width, height int) string

	// OnPush fires when the overlay is added to the stack (e.g. user
	// typed `/btw question`). May return a Cmd that does the async work
	// (LLM call, file load, etc).
	OnPush() tea.Cmd

	// OnPop fires when the overlay is removed (Esc or programmatic).
	// Use it to release resources or cancel in-flight Cmds.
	OnPop() tea.Cmd
}

// Stack owns overlay z-order + focus routing. The "top" of the stack is
// the most-recently-pushed Active overlay; that one gets keyboard
// events. Inactive overlays in the middle are skipped silently.
//
// Concurrency: not goroutine-safe by design. The bubbletea Update loop
// is single-threaded, so the Stack only ever sees mutations from Update
// callbacks and keystrokes — no locks needed. If we ever need cross-
// goroutine pushes we'll wrap in a mutex then.
type Stack struct {
	overlays []Overlay
}

// New returns an empty stack.
func New() *Stack { return &Stack{} }

// Push adds an overlay to the top of the stack and returns its OnPush
// command. If an overlay with the same Name() is already on the stack,
// the existing one is replaced (rather than allowing duplicates) — this
// matches claude-code's "open modal again = reset its state" UX.
//
// Nil receiver: tests that hand-construct Model bypass NewModel, so a
// nil Stack would panic on the first Push. Treat nil as no-op.
func (s *Stack) Push(o Overlay) tea.Cmd {
	if s == nil || o == nil {
		return nil
	}
	// Replace duplicate by name if present.
	for i, existing := range s.overlays {
		if existing.Name() == o.Name() {
			popCmd := existing.OnPop()
			s.overlays[i] = o
			pushCmd := o.OnPush()
			return tea.Batch(popCmd, pushCmd)
		}
	}
	s.overlays = append(s.overlays, o)
	return o.OnPush()
}

// Pop removes the topmost Active overlay and returns its OnPop command.
// No-op when no Active overlay is on the stack. Nil-safe.
func (s *Stack) Pop() tea.Cmd {
	if s == nil {
		return nil
	}
	for i := len(s.overlays) - 1; i >= 0; i-- {
		if s.overlays[i].Active() {
			cmd := s.overlays[i].OnPop()
			s.overlays = append(s.overlays[:i], s.overlays[i+1:]...)
			return cmd
		}
	}
	return nil
}

// PopByName removes a specific overlay regardless of position. Used when
// a Cmd completes (e.g. /btw answer arrives) and the overlay wants to
// dismiss itself by sending a `dismissByNameMsg`. Nil-safe.
func (s *Stack) PopByName(name string) tea.Cmd {
	if s == nil {
		return nil
	}
	for i, o := range s.overlays {
		if o.Name() == name {
			cmd := o.OnPop()
			s.overlays = append(s.overlays[:i], s.overlays[i+1:]...)
			return cmd
		}
	}
	return nil
}

// Top returns the top Active overlay, or nil. Inactive overlays are
// transparent to the focus system. Nil-safe.
func (s *Stack) Top() Overlay {
	if s == nil {
		return nil
	}
	for i := len(s.overlays) - 1; i >= 0; i-- {
		if s.overlays[i].Active() {
			return s.overlays[i]
		}
	}
	return nil
}

// Get returns the overlay matching name (whether active or not), or nil.
// Useful for letting the host poke into a specific overlay's state from
// outside (e.g. delivering an async result via a custom Cmd). Nil-safe.
func (s *Stack) Get(name string) Overlay {
	if s == nil {
		return nil
	}
	for _, o := range s.overlays {
		if o.Name() == name {
			return o
		}
	}
	return nil
}

// Replace swaps the in-stack overlay with name for `o` (preserving
// position). When no match exists this is equivalent to Push. Returns
// the OnPop of the old + OnPush of the new, batched. Nil-safe.
func (s *Stack) Replace(name string, o Overlay) tea.Cmd {
	if s == nil {
		return nil
	}
	for i, existing := range s.overlays {
		if existing.Name() == name {
			popCmd := existing.OnPop()
			s.overlays[i] = o
			pushCmd := o.OnPush()
			return tea.Batch(popCmd, pushCmd)
		}
	}
	return s.Push(o)
}

// Active returns true when there's any Active overlay (so the host
// knows to dim the chat surface, suppress some keystrokes, etc).
// Nil-safe (returns false).
func (s *Stack) Active() bool { return s != nil && s.Top() != nil }

// Update routes a key to the topmost Active overlay. Returns the cmd
// from that overlay's Update, plus a `consumed` flag the host uses to
// decide whether to also let the chat input see the key. Nil-safe.
func (s *Stack) Update(msg tea.KeyMsg) (cmd tea.Cmd, consumed bool) {
	if s == nil {
		return nil, false
	}
	top := s.Top()
	if top == nil {
		return nil, false
	}
	next, c, consumed := top.Update(msg)
	if next != nil {
		// Find and replace top in place; the overlay may have produced
		// a new value (immutable-update pattern, common in Go).
		for i := len(s.overlays) - 1; i >= 0; i-- {
			if s.overlays[i] == top {
				s.overlays[i] = next
				break
			}
		}
	}
	return c, consumed
}

// View renders every Active overlay top-to-bottom (caller stacks them
// in their final string composition). Inactive overlays produce no
// output. width/height come from the terminal size. Nil-safe.
func (s *Stack) View(width, height int) []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.overlays))
	for _, o := range s.overlays {
		if !o.Active() {
			continue
		}
		v := o.View(width, height)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Len returns the number of overlays on the stack (active or not).
// Nil-safe (returns 0).
func (s *Stack) Len() int {
	if s == nil {
		return 0
	}
	return len(s.overlays)
}
