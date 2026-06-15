# bubbletea v2: blank first frame when `GetSize` returns 0 (e.g. tmux)

This is a write-up of an upstream bug in `charm.land/bubbletea/v2`
(v2.0.6), the metis client-side mitigation, and a ready-to-submit PR for
bubbletea.

## Symptom

Launching a bubbletea v2 program inside tmux (and some other pty
setups) shows a **blank screen** until the first event — the user
resizes the window or presses a key — at which point the UI finally
paints. It looks like the program hung. metis hit this on both the chat
surface and the one-shot "trust this folder?" prompt.

## Root cause (confirmed in bubbletea source)

`Program.Run` reads the terminal size once at startup and trusts it
without guarding against a zero result:

```go
// tea.go (v2.0.6), ~line 1043
if p.ttyOutput != nil {
    w, h, err := term.GetSize(p.ttyOutput.Fd())
    if err != nil {
        return p.initialModel, fmt.Errorf("bubbletea: error getting terminal size: %w", err)
    }
    width, height = w, h            // <-- no check for w==0 / h==0
}
p.width, p.height = width, height
resizeMsg := WindowSizeMsg{Width: p.width, Height: p.height}
// ...
go p.Send(resizeMsg)               // initial WindowSizeMsg = {0,0}
p.renderer.resize(resizeMsg.Width, resizeMsg.Height)  // renderer viewport = 0x0
```

Under tmux the pty size often isn't negotiated yet at this instant, so
`term.GetSize` returns `(0, 0)` **with `err == nil`**. bubbletea then
resizes its renderer to `0x0`. Because the event loop only renders
*after* it receives a message (`eventLoop` blocks on `<-p.msgs`, then
calls `p.render(model)`), and the only startup message is the bogus
`WindowSizeMsg{0,0}`, the renderer paints into a 0x0 viewport and the
frame is clipped to nothing. The screen stays blank until a real
`SIGWINCH` (resize) or any other event arrives and the renderer gets a
non-zero size.

Note this is independent of the model's `View()` — even a `View()` that
ignores width entirely (metis's trust prompt) renders blank, because the
*renderer viewport* is 0x0.

## Proposed bubbletea fix (the PR)

Fall back to a sane default when `GetSize` yields 0, mirroring what every
terminal app does:

```go
if p.ttyOutput != nil {
    w, h, err := term.GetSize(p.ttyOutput.Fd())
    if err != nil {
        return p.initialModel, fmt.Errorf("bubbletea: error getting terminal size: %w", err)
    }
    // Some terminals (notably tmux before the pty size is negotiated)
    // return 0x0 from the initial ioctl with no error. Rendering into a
    // 0x0 viewport blanks the first frame until a real SIGWINCH. Fall
    // back to a standard 80x24 so the first frame paints; the real size
    // arrives via SIGWINCH a moment later and re-renders correctly.
    if w <= 0 || h <= 0 {
        w, h = 80, 24
    }
    width, height = w, h
}
```

PR title: `fix(tea): fall back to 80x24 when GetSize returns 0 at startup (blank first frame under tmux)`

Reproduction for the PR: any minimal bubbletea v2 program launched in a
detached/early tmux pane renders blank until the first event; with the
fallback it paints immediately.

## metis client-side mitigation (shipped, doesn't need the PR)

Since we can't wait on upstream, metis mitigates it itself:

- `internal/tui` `Model.Init()` and `cmd/metis/trust.go` `trustModel.Init()`
  return a short `tea.Tick` that re-issues `tea.RequestWindowSize()`
  ~60ms after start. By then tmux has negotiated the pty, so the
  re-query returns the real size and the renderer resizes + paints.
- `Model` is constructed with a default `width:80, height:24`, and the
  `WindowSizeMsg` handler ignores a `0x0` message, so the model layout
  never collapses to width 0 even before the real size arrives.
