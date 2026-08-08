# Metis Ultraviolet patch

This directory is Ultraviolet
`v0.0.0-20260703014108-f5a850f9c2b7`
(`h1:3FmWoGNWK4STvqg0O0Aeav2T7rodWJAPeF0QpH+8gFw=`) plus one Metis runtime patch
and its test adaptations:

- `terminal_renderer.go` uses absolute `CUP` positioning for every vertical
  move in fullscreen mode. The upstream byte-cost optimizer can otherwise use
  bare `LF` or `RI` (`ESC M`); at a physical screen margin those operations
  scroll iTerm2 instead of only moving the cursor. A width/autowrap mismatch
  can therefore leave an old frame above a newly rendered frame even when
  hard-scroll optimization is disabled.
- Renderer golden outputs now expect fullscreen `CUP` movements, and
  `poll_default_test.go` orders cancellation before reading so race-enabled
  macOS CI cannot consume a ready pipe byte before the cancellation assertion.

When upgrading Ultraviolet, first replace this directory with the exact target
module source, then reapply the change and run both this module's tests and
Metis's TUI renderer regression tests.
