# Metis Bubble Tea patch

This directory is Bubble Tea `v2.0.8`
(`h1:SxTJMhCAI3lbPmy4SgX5LWZ24AdINr4I6UEqzZvYJuY=`) plus three Metis patches,
and its `go.mod` points at the adjacent patched Ultraviolet module:

- `cursed_renderer.go` disables Ultraviolet fullscreen scroll optimization.
  Direct iTerm2 can apply its whole-line `IL`/`DL`/`SU`/`SD` operations at a
  stale physical cursor after a burst of background-agent rows, leaving an old
  frame above the current frame. Ordinary cell-level diff rendering remains
  enabled.
- `tea.go` gives each renderer flush goroutine its own ticker. This prevents a
  `ReleaseTerminal`/`RestoreTerminal` race where the old goroutine can stop the
  shared ticker after `RestoreTerminal` has reset it for the restored renderer,
  permanently halting screen updates.
- `tea.go` synchronously flushes a pending View before handling `tea.Println`.
  This guarantees a model that switches from alt-screen to inline copy mode in
  the same update emits `DECRST 1049` before its transcript, so long histories
  land completely in the terminal's native scrollback instead of being clipped
  inside the old alternate buffer.

When upgrading Bubble Tea, first replace this directory with the exact target
module source, then reapply all three changes and run both this module's tests and
Metis's TUI renderer regression tests.
