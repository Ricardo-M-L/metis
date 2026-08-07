# Metis Bubble Tea patch

This directory is Bubble Tea `v2.0.8`
(`h1:SxTJMhCAI3lbPmy4SgX5LWZ24AdINr4I6UEqzZvYJuY=`) plus one Metis patch:

- `cursed_renderer.go` disables Ultraviolet fullscreen scroll optimization.
  Direct iTerm2 can apply its whole-line `IL`/`DL`/`SU`/`SD` operations at a
  stale physical cursor after a burst of background-agent rows, leaving an old
  frame above the current frame. Ordinary cell-level diff rendering remains
  enabled.

When upgrading Bubble Tea, first replace this directory with the exact target
module source, then reapply the one renderer change and run both this module's
tests and Metis's TUI renderer regression tests.
