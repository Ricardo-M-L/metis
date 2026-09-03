# METIS Desktop message layout QA

- Source visual truth: `/var/folders/gm/w8ft20ns0w1fkv83pkrvj80jylj1v8/T/codex-clipboard-2b445ca1-99e1-4ef7-a648-da0488df4335.png`
- Implementation screenshot: `/var/folders/gm/w8ft20ns0w1fkv83pkrvj80jylj1v8/T/com.openai.sky.CUAService/METIS-QA Screenshot 2026-09-03 at 6.04.34 PM.jpeg`
- Viewport: maximized macOS native Wails window
- Source pixels: 2536 x 1464; implementation pixels: 1296 x 768
- CSS size and density normalization: the source is a Retina-density capture and the implementation is a native screen capture. They were compared proportionally at their full captured sizes in one comparison input; no pixel-level measurement was inferred across densities.
- State: dark theme, the same `你好 (branch)` history session, with user bubbles, assistant replies, action metadata, and the native title bar visible.

## Full-view comparison evidence

The repaired build preserves the existing sidebar, transcript width, typography, colors, bubbles, tool rows, composer, and status bar. The previously missing native zoom control is present as the third macOS traffic-light button and its native `zoom the window` action successfully maximizes the window.

## Focused comparison evidence

The title-bar controls and the transcript region were inspected at full resolution because their icons and spacing are too small for a reduced overview. User action metadata now sits below the bubble and shares its right edge. The effective rhythm is 8px after a user message and 32px after an assistant message, making one prompt/reply group visibly tighter than the gap before the next prompt.

## Findings and comparison history

### Initial findings

- P1: macOS zoom/maximize control was disabled because Wails received no explicit macOS options.
- P1: user action metadata was a horizontal sibling of the bubble, so it rendered to the bubble's right.
- P2: user and assistant rows both used the same 16px bottom margin, so completed turns had no grouping hierarchy.

### Fixes made

- Added explicit macOS window options with zoom enabled while retaining resizing.
- Changed the canonical user row to a right-aligned column so actions render below the bubble.
- Set an 8px prompt-to-reply gap and a 32px completed-turn gap.

### Post-fix evidence

- The native accessibility tree exposes close, full-screen/zoom, and minimize controls; invoking the zoom action maximized the window.
- The implementation screenshot shows user actions and the timestamp beneath the user bubble.
- The implementation screenshot shows a larger gap after the assistant response than after the following user prompt.
- Fonts and typography: unchanged from the existing METIS design system; no wrapping or hierarchy regression observed.
- Colors and tokens: unchanged; the dark theme and semantic error/tool colors remain consistent.
- Image and asset quality: no image assets changed in this fix.
- Copy and content: unchanged; existing Chinese messages and timestamps remain intact.

No actionable P0, P1, or P2 visual findings remain.

final result: passed
