# Metis queued-message card design QA

## Evidence

- Source visual truth: `/var/folders/gm/w8ft20ns0w1fkv83pkrvj80jylj1v8/T/codex-clipboard-e87ae15c-3651-437b-979b-8dd7fe53e358.png`
- Source pixels: 920 × 310
- Default implementation screenshot: `/tmp/metis-design-qa/queue-default-full.png`
- Default viewport and screenshot pixels: 1280 × 720, device scale 1
- Focused implementation crop: `/tmp/metis-design-qa/queue-default-component-v2.png`
- Focused implementation pixels: 800 × 305
- Narrow implementation screenshot: `/tmp/metis-design-qa/queue-narrow-full.png`
- Narrow viewport and screenshot pixels: 600 × 720, device scale 1
- Combined comparison: `/tmp/metis-design-qa/queue-comparison.png`
- Comparison normalization: source and focused implementation were scaled to the same 310 px comparison height. The combined PNG was encoded at Retina 2×, with both sides scaled equally.
- State: dark theme, one text-only queued message, running turn, actions menu open.

## Findings

- No actionable P0, P1, or P2 differences remain in the queued-message component.
- Fonts and typography: the card uses the product's existing system font stack, 16 px semibold message text, and 15 px semibold menu actions. The hierarchy matches the reference while staying consistent with Metis Desktop.
- Spacing and layout rhythm: the queue is now a full-width 62 px card with a 24 px radius, leading queue mark, flexible message column, steer action, delete action, and circular overflow action. The 152 px menu keeps the same three-action rhythm as the reference.
- Colors and visual tokens: the component uses Metis surface, border, foreground, hover, focus, and shadow tokens. Contrast remains consistent in dark mode.
- Image and icon fidelity: all new icons are standalone Lucide assets with round strokes; no placeholder, emoji, CSS drawing, or handcrafted inline icon is used.
- Copy and content: Chinese actions match the reference: “调整方向”, “编辑消息”, “在侧边聊天中打开”, and “关闭排队”. English labels are provided through the existing language switch.
- Expected product differences: Metis keeps its compact composer and existing 780 px conversation width. When the default-height viewport cannot fit the menu below the card, the menu opens upward to remain fully visible; at the narrow tested viewport it opens downward and remains inside the viewport.

## Interaction verification

- Queue rendering: verified with a real running Desktop turn.
- Adjust direction: verified against `/api/steer`; the queued card was removed and the user correction appeared in the active transcript.
- Edit message: verified; the text moved back into the composer and the queue card disappeared. The automated harness also verifies that an existing composer draft is swapped into the queue instead of being lost.
- Delete: verified; the selected queued message disappeared.
- Open in side chat: verified against `/api/fork`; a `(branch)` session opened and the queued text appeared as its composer draft.
- Close queue: verified; all queued messages were cleared.
- Menu placement: verified at 1280 × 720 and 600 × 720.
- Browser console: 0 warnings and 0 errors during the final run.

## Comparison history

1. Initial default-height capture found a P1 menu overflow: the downward menu extended below the viewport. Fixed by measuring the rendered card and menu and opening upward only when the menu would clip and there is room above. Post-fix evidence: `queue-default-full.png`.
2. Initial 600 px capture found a P2 responsive wrap: the overflow button occupied an unintended second grid row, expanding the card to 100 px. Fixed the narrow layout to retain all five grid tracks and hide only the “调整方向” text label. Post-fix evidence: `queue-narrow-full.png`; measured card height is 62 px.

## Implementation checklist

- [x] Replace the old count-and-pill queue presentation.
- [x] Match the Codex card hierarchy and menu styling.
- [x] Wire every visible primary action to working behavior.
- [x] Preserve drafts and image attachments during edit/branch flows.
- [x] Keep menus inside the viewport.
- [x] Verify the narrow breakpoint.
- [x] Run browser interaction and console checks.

final result: passed
