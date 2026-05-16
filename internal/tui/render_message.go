package tui

// render_message.go — single-message rendering for the transcript:
// user prompts, assistant replies (with markdown), thought summaries,
// recap lines, info/error banners. Tool events live in render_tool.go.

import (
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
)

// renderMessage prints a single transcript row. Spacing convention:
// each major-turn boundary (user prompt, assistant reply) gets a
// trailing blank line so consecutive turns visually separate; metadata
// rows (thought-summary, recap, info) hug their parent reply with a
// single newline. claude-code parity — the previous tight-packed style
// read as cramped on terminals with limited contrast.
//
// width is the available render width — passed through so the
// assistant-body markdown renderer can word-wrap at the right column
// instead of the raw terminal width.
// renderMessage prints a single transcript row. expand=true means the
// caller wants verbose rendering — extended thinking unfolded, otherwise
// thinking collapses to a one-liner so the chat surface isn't drowned
// in dim italic. Mirrors claude-code's
// AssistantThinkingMessage shouldShowFullThinking gate (controlled by
// transcript-mode + verbose flag there; metis reuses Ctrl+O's
// expandToolOutputs toggle for both tool output and thinking).
func renderMessage(msg Message, width int, expand bool) string {
	var s strings.Builder
	switch msg.Role {
	case "user":
		// User prompt opens a new turn — leading blank above and
		// trailing blank below so the eye lands on the whole turn
		// as a discrete block.
		s.WriteString("\n")
		s.WriteString(styleUser.Render("  " + glyphPrompt + " " + msg.Content))
		s.WriteString("\n")
		// Pasted-image attachments: claude-code prints one indented
		// `└ [Image #N]` row per placeholder underneath the prompt so
		// the user has a visible "yes, the agent received the image"
		// confirmation. We pull the tags directly from the rendered
		// text (placeholders are 1:1 with attachment content blocks
		// emitted in handleSubmit, so this is accurate even though the
		// renderer can't see the agent loop's Messages slice).
		for _, m := range pastedImageTag.FindAllString(msg.Content, -1) {
			s.WriteString(styleMuted.Render("    " + glyphTreeLeaf + "  "))
			s.WriteString(styleAccent.Render(m))
			s.WriteString("\n")
		}
	case "assistant":
		// Assistant reply sits visually beneath the user prompt;
		// indented bullet + body, trailing blank to separate from
		// the next turn or follow-up tool/recap rows.
		//
		// renderAssistantBody returns multi-line markdown; only the
		// first line gets the bullet, but every continuation line
		// must keep the same 2-col left margin or the body bleeds
		// flush against the terminal edge (image #48). Walk the
		// body line-by-line and prefix continuations with "  ".
		body := renderAssistantBody(msg.Content, width)
		bodyLines := strings.Split(body, "\n")
		s.WriteString("  ")
		s.WriteString(styleAsst.Render(glyphBullet + " "))
		if len(bodyLines) > 0 {
			s.WriteString(bodyLines[0])
			for _, ln := range bodyLines[1:] {
				s.WriteString("\n  ")
				s.WriteString(ln)
			}
		}
		s.WriteString("\n\n")
	case "thinking":
		// Extended-thinking trace. By default we collapse to a single
		// dim italic line so the transcript stays readable; the user
		// can press Ctrl+O to unfold all thinking + tool output at
		// once (claude-code's verbose flow). Without this gate, a
		// chatty model floods the screen with grey italic that
		// visually competes with the actual reply (image #17 user
		// feedback 2026-05-10).
		// Glyph in cyan accent (matches the `thought-summary` row's
		// styleAccent below at line ~111 — visual consistency between
		// streaming "thinking" and turn-end "Tinkered for Xs"). Body
		// text in styleDim (#a0a0a0) instead of styleMuted (#606060)
		// because claude-code's AssistantThinkingMessage uses
		// `dimColor` (ANSI Faint, ~50% of fg) rather than fixed
		// ultra-grey — readable on dark themes, doesn't disappear.
		s.WriteString(styleAccent.Render("  " + glyphAsterisk + " "))
		thinkStyle := styleDim.Italic(true)
		if !expand {
			// Folded view — first line preview only, plus a hint when
			// the row has room. Width-aware so the hint never wraps
			// onto a second row over the next chat item (image #7/#8).
			first := firstThinkingLine(msg.Content, width)
			s.WriteString(thinkStyle.Render(first))
			if thinkingHintFits(width) {
				s.WriteString(styleMuted.Render("  "))
				s.WriteString(styleMuted.Render("(ctrl+o to expand)"))
			}
			s.WriteString("\n")
		} else {
			thinkLines := strings.Split(msg.Content, "\n")
			if len(thinkLines) > 0 {
				s.WriteString(thinkStyle.Render(thinkLines[0]))
				for _, ln := range thinkLines[1:] {
					s.WriteString("\n  ")
					s.WriteString(thinkStyle.Render(ln))
				}
			}
			s.WriteString("\n")
		}
	case "redacted_thinking":
		// Anthropic safety classifier replaced this reasoning chunk
		// with opaque cipher text. The cipher text is in msg.Content
		// but MUST NOT be displayed — it's only kept so the next turn
		// can echo it back to Anthropic for decryption. Render a
		// distinct placeholder so the user knows redaction happened
		// without ever seeing the encrypted bytes. No ctrl+o expand
		// path: there's no plaintext to expand into.
		//
		// Glyph uses the lock emoji + accent colour so the row stands
		// out from normal thinking, mirroring CC's redacted-thinking
		// affordance (CC shows "🔒 [encrypted reasoning]" in the same
		// transcript position).
		s.WriteString(styleAccent.Render("  🔒 "))
		s.WriteString(styleDim.Italic(true).Render("[thinking redacted by Anthropic safety classifier — encrypted, model can still use it next turn]"))
		s.WriteString("\n")
	case "thought-summary":
		// "✻ Cogitated for 1m 32s" — render the glyph in the accent
		// color (it's a category marker, like claude-code's flower
		// glyph) and the body in dim secondary so the duration stays
		// readable. Earlier this whole row was muted grey, which
		// made every turn end with an invisible footer.
		s.WriteString(styleAccent.Render("  " + glyphAsterisk + " "))
		s.WriteString(styleDim.Render(msg.Content))
		s.WriteString("\n")
	case "compaction":
		s.WriteString(styleAccent.Render("  " + glyphAsterisk + " "))
		s.WriteString(styleDim.Render("Conversation compacted "))
		s.WriteString(styleMuted.Render("(" + msg.Content + ")"))
		s.WriteString("\n")
	case "recap":
		// Structural turn recap: useful for skimming what the model
		// just did. Bumped from muted to dim so it's actually readable
		// — when this row was muted grey it visually fused with the
		// prior thought-summary and got skipped.
		s.WriteString(styleAccent.Render("  " + glyphRecap + " "))
		s.WriteString(styleDim.Render("recap: " + msg.Content))
		s.WriteString("\n")
	case "error":
		s.WriteString(styleErr.Render("  ✗ " + msg.Content))
		s.WriteString("\n")
	case "error-hint":
		// Recovery hint sits one line under the red error in dim
		// secondary so the user reads "what went wrong" then "what
		// to do" without the hint screaming for attention. claude-
		// code does the same: error in red, suggestion below.
		s.WriteString(styleDim.Render("    → " + msg.Content))
		s.WriteString("\n")
	case "success":
		// Phase B: ✓ glyph in green for celebratory confirmations
		// (saved, exported, branched, undid). Distinct from neutral
		// "info" — the user wants visible feedback that an action
		// landed, not just another grey line in the scroll.
		s.WriteString(styleSuccess.Render("  ✓ " + msg.Content))
		s.WriteString("\n")
	case "warning":
		// ⚠ in yellow for soft warnings (no session store, deprecated
		// usage). Visible without screaming like "error".
		s.WriteString(styleWarn.Render("  ⚠ " + msg.Content))
		s.WriteString("\n")
	case "bash", "bash-error":
		// `!ls` mode output. First line ($ <cmd>) gets accent so the
		// user can spot their own shell invocation against the dim
		// stdout that follows. Errors render in red instead of dim
		// for the body, so a non-zero exit pops without us having to
		// add a separate ✗ glyph (the `(exit: ...)` tail tells you).
		bodyStyle := styleDim
		if msg.Role == "bash-error" {
			bodyStyle = styleErr
		}
		lines := strings.Split(msg.Content, "\n")
		for i, ln := range lines {
			if i == 0 {
				// $ <cmd> header line.
				s.WriteString(styleAccent.Render("  ▸ "))
				s.WriteString(styleAsst.Render(strings.TrimPrefix(ln, "$ ")))
			} else {
				s.WriteString("    ")
				s.WriteString(bodyStyle.Render(ln))
			}
			s.WriteString("\n")
		}
	case "info":
		// Phase B: subtle "·" prefix gives info messages a consistent
		// left edge with the other roles (✗ for error, ✓ for success,
		// ⚠ for warning). Without the prefix the eye lost the column
		// and info messages floated awkwardly.
		s.WriteString(styleMuted.Render("  · " + msg.Content))
		s.WriteString("\n")
	case "user-steer":
		// Mid-turn user input that was injected via SteerInject (the
		// user typed something while the previous turn was still
		// running). Visually the same lane as a fresh user prompt so
		// the user sees their query land — but with a steer arrow to
		// distinguish it from a turn-starting prompt.
		s.WriteString("\n")
		s.WriteString(styleUser.Render("  ↳ " + msg.Content))
		s.WriteString("\n")
	}
	return s.String()
}

// markdownRenderer is the lazily-built glamour TermRenderer used for
// assistant-message bodies. We cache it because constructing glamour
// is expensive (loads styles, parses templates) and the renderer is
// stateless once built. Recreate when terminal width changes more than
// 8 cols since glamour's word-wrap is baked at construction time.
//
// Two flavours are cached separately, keyed by `wide`:
//   - narrow (wide=false, cap 120): prose-friendly wrap for plain
//     paragraphs / bullet lists — matches claude-code's reading column.
//   - wide   (wide=true,  no cap):  full terminal width for messages
//     that contain markdown tables. glamour sizes the lipgloss table
//     to WordWrap (see ansi/blockstack.go:Width), so capping at 120
//     squashed 6+ column CJK comparison tables into multi-line "half
//     tables". Tables get the full terminal width; prose still wraps
//     because glamour wraps each paragraph at WordWrap regardless.
var (
	mdRendererMu             sync.Mutex
	mdRendererNarrow         *glamour.TermRenderer
	mdRendererNarrowForWidth int
	mdRendererWide           *glamour.TermRenderer
	mdRendererWideForWidth   int
)

func getMarkdownRenderer(width int, wide bool) *glamour.TermRenderer {
	mdRendererMu.Lock()
	defer mdRendererMu.Unlock()

	cached := mdRendererNarrow
	cachedWidth := mdRendererNarrowForWidth
	if wide {
		cached = mdRendererWide
		cachedWidth = mdRendererWideForWidth
	}
	if cached != nil && abs(width-cachedWidth) < 8 {
		return cached
	}

	wrap := width
	if wrap < 40 {
		wrap = 40
	}
	if !wide && wrap > 120 {
		wrap = 120
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(metisCodeBlockStyle()),
		glamour.WithWordWrap(wrap),
		glamour.WithTableWrap(true),
	)
	if err != nil {
		return nil
	}
	if wide {
		mdRendererWide = r
		mdRendererWideForWidth = width
	} else {
		mdRendererNarrow = r
		mdRendererNarrowForWidth = width
	}
	return r
}

// metisCodeBlockStyle returns glamour's stock dark style with all
// red-ish chroma colours scrubbed.
//
// glamour's default dark theme paints:
//   - chroma.Error.BackgroundColor = #F05B5B — hot-pink fill on every
//     token the lexer can't classify. ASCII box-drawing chars
//     (┌─┐│└┘├┤┬┴┼) used in directory trees + architecture diagrams
//     fall here, so users see whole walls of red over clean diagram
//     art (images #13 + #14 2026-05-07).
//   - chroma.GenericDeleted.Color = #FD5B5B — leaks onto regular `-`
//     lines in non-diff blocks (markdown bullets inside code fences).
//   - chroma.Operator / KeywordNamespace / KeywordReserved /
//     NameBuiltin / CommentPreproc — all set to red/pink/orange
//     shades that compound the visual chaos when the lexer mis-
//     tokenises ASCII art.
//
// We zero-out every offending token. Other dark-theme colours
// preserved — strings, regular keywords, numbers, comments stay
// readable; only the bath-of-red goes away.
func metisCodeBlockStyle() ansi.StyleConfig {
	cfg := styles.DarkStyleConfig
	chroma := *cfg.CodeBlock.Chroma
	zero := ansi.StylePrimitive{}
	chroma.Error = zero
	chroma.GenericDeleted = zero
	chroma.Operator = zero
	chroma.KeywordNamespace = zero
	chroma.KeywordReserved = zero
	chroma.NameBuiltin = zero
	chroma.CommentPreproc = zero
	cfg.CodeBlock.Chroma = &chroma

	// Force ASCII table separators. glamour DarkStyleConfig leaves
	// CenterSeparator/ColumnSeparator/RowSeparator nil, so lipgloss
	// falls back to NormalBorder which uses `─ │ ┼` — all East Asian
	// "Ambiguous" width chars. lipgloss sizes columns using narrow=1,
	// but on CN/JP/KR locale terminals each renders as 2 cells, so the
	// separator row visibly doubles in width and overflows the line.
	// Effect: tables look like "half tables" (image #1 user feedback
	// 2026-05-15). ASCII `- | +` are 1 cell in every locale so the
	// table never overflows the column allocation lipgloss computed.
	dash := "-"
	pipe := "|"
	plus := "+"
	cfg.Table.RowSeparator = &dash
	cfg.Table.ColumnSeparator = &pipe
	cfg.Table.CenterSeparator = &plus
	return cfg
}

// renderAssistantBody pretty-prints the assistant text with markdown
// support — code blocks get syntax-highlighted boxes, bold/italic
// render as ANSI styles, links get OSC-8 hyperlinks. Falls back to
// plain styled text when:
//   - the content has no markdown markers (no need to pay the parsing cost)
//   - glamour fails (graceful degradation, e.g. on weird input)
//
// Width-aware: passes the available render width so glamour wraps
// inside the chat-surface column rather than the raw terminal width.
func renderAssistantBody(content string, width int) string {
	if !looksLikeMarkdown(content) {
		return styleAsst.Render(content)
	}

	// Fast path: no table in the message, render the whole thing
	// through glamour as before.
	segs, hasTable := splitMarkdownTables(content)
	if !hasTable {
		r := getMarkdownRenderer(width-4, false)
		if r == nil {
			return styleAsst.Render(content)
		}
		out, err := r.Render(content)
		if err != nil {
			return styleAsst.Render(content)
		}
		return strings.TrimSpace(out)
	}

	// Mixed path: prose segments through glamour, tables through our
	// own lipgloss-based renderer with full ASCII frame + header band.
	// glamour gets the narrow (prose-friendly 120 cap) renderer here
	// because the table is rendered separately and won't be squashed.
	r := getMarkdownRenderer(width-4, false)
	tableWidth := width - 4
	if tableWidth < 20 {
		tableWidth = 20
	}

	var sb strings.Builder
	for i, seg := range segs {
		if i > 0 {
			sb.WriteString("\n")
		}
		switch seg.kind {
		case segText:
			body := strings.TrimSpace(seg.text)
			if body == "" {
				continue
			}
			if r == nil {
				sb.WriteString(styleAsst.Render(body))
				continue
			}
			out, err := r.Render(body)
			if err != nil {
				sb.WriteString(styleAsst.Render(body))
				continue
			}
			sb.WriteString(strings.TrimSpace(out))
		case segTable:
			sb.WriteString(renderMetisTable(seg.headers, seg.rows, tableWidth))
		}
	}
	return strings.TrimSpace(sb.String())
}

// containsMarkdownTable looks for a GFM-style table header — a row with
// at least one pipe followed on the next line by a separator row using
// dashes (`---` / `:---:` etc). Cheap textual scan, no parser. Used to
// route wide-table messages to the no-cap markdown renderer.
func containsMarkdownTable(s string) bool {
	lines := strings.Split(s, "\n")
	for i := 0; i < len(lines)-1; i++ {
		if !strings.Contains(lines[i], "|") {
			continue
		}
		sep := strings.TrimSpace(lines[i+1])
		if sep == "" || !strings.Contains(sep, "|") {
			continue
		}
		body := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(sep, "|", ""), ":", ""), " ", "")
		if len(body) >= 3 && strings.Trim(body, "-") == "" {
			return true
		}
	}
	return false
}

// looksLikeMarkdown is a cheap heuristic that skips glamour for plain
// short replies ("ok", "done"). Without this, every "ok" assistant
// message goes through markdown parsing, which is wasteful. Triggers
// on the obvious markers: code fences, headers, bold/italic, lists,
// and inline code.
func looksLikeMarkdown(s string) bool {
	if len(s) > 80 {
		return true
	}
	for _, marker := range []string{"```", "**", "__", "# ", "## ", "- ", "* ", "1. ", "`", "[", "> "} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	// Short GFM tables can miss every marker above ("| a | b |\n|---|---|")
	// — detect them explicitly so the table renderer still kicks in for
	// terse 2-column comparisons. Cheap textual scan, no parse cost.
	return containsMarkdownTable(s)
}

// firstThinkingLine returns a single-line preview suitable for the
// folded thinking view: the first non-empty line of content, capped
// so that the rendered row "  ✻ <text>  (ctrl+o to expand)" fits in
// `width` columns without wrapping. Empty input yields "Thinking…"
// so the user sees SOMETHING (the glyph alone is too easy to miss
// in a long transcript).
//
// width<=0 falls back to the historical 80-col cap (covers tests
// and the rare cold-render path before WindowSizeMsg arrives).
//
// Reflow note: claude-code's wrap-text re-measures every frame
// against the current terminal columns. metis caches by width in
// renderCache, so we just need the budget here to track the live
// width. Without this, shrinking the terminal left the hint
// floating over content (user reports image #7/#8 2026-05-10).
func firstThinkingLine(content string, width int) string {
	const (
		leftMargin = 4  // "  ✻ "
		hintWidth  = 20 // "  (ctrl+o to expand)"
		minBody    = 12 // anything narrower → drop the hint and use full width
	)
	// Budget is in terminal CELLS (not runes), so CJK content gets
	// the same one-row guarantee as ASCII. Without this, "用户问的是..."
	// at 60 cols would land at 60+ cells and wrap, even though the
	// rune count fit.
	budget := width - leftMargin - hintWidth
	switch {
	case width <= 0:
		// Cold-render path (tests, pre-WindowSizeMsg). Stay rune-based
		// so the legacy 80-rune contract holds.
		for _, ln := range strings.Split(content, "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" {
				continue
			}
			return truncateRunes(ln, 80)
		}
		return "Thinking…"
	case budget < minBody:
		// Narrow terminal: hint won't fit; use full width minus a safety col.
		budget = width - leftMargin - 1
		if budget < minBody {
			budget = minBody
		}
	}
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		return truncateCells(ln, budget)
	}
	return "Thinking…"
}

// thinkingHintFits reports whether the "(ctrl+o to expand)" suffix
// fits next to a folded thinking preview at the given width. Callers
// drop the hint when this is false; otherwise it would wrap onto a
// new row and orphan over the content (image #7/#8 user reports).
func thinkingHintFits(width int) bool {
	const (
		leftMargin = 4
		hintWidth  = 20
		minBody    = 12
	)
	if width <= 0 {
		return true // historical default, used by tests
	}
	return width-leftMargin-hintWidth >= minBody
}
