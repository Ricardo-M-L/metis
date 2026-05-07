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
func renderMessage(msg Message, width int) string {
	var s strings.Builder
	switch msg.Role {
	case "user":
		// User prompt opens a new turn — leading blank above and
		// trailing blank below so the eye lands on the whole turn
		// as a discrete block.
		s.WriteString("\n")
		s.WriteString(styleUser.Render("  " + glyphPrompt + " " + msg.Content))
		s.WriteString("\n")
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
		// Extended-thinking trace — claude-code shows it dim/italic
		// with ✻ glyph. Keeps the user informed about model
		// reasoning without competing with the final answer style.
		// Continuation lines indent under the glyph for the same
		// reason as the assistant body.
		s.WriteString(styleMuted.Render("  " + glyphAsterisk + " "))
		thinkStyle := styleMuted.Italic(true)
		thinkLines := strings.Split(msg.Content, "\n")
		if len(thinkLines) > 0 {
			s.WriteString(thinkStyle.Render(thinkLines[0]))
			for _, ln := range thinkLines[1:] {
				s.WriteString("\n  ")
				s.WriteString(thinkStyle.Render(ln))
			}
		}
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
var (
	mdRendererMu       sync.Mutex
	mdRenderer         *glamour.TermRenderer
	mdRendererForWidth int
)

func getMarkdownRenderer(width int) *glamour.TermRenderer {
	mdRendererMu.Lock()
	defer mdRendererMu.Unlock()
	if mdRenderer != nil && abs(width-mdRendererForWidth) < 8 {
		return mdRenderer
	}
	wrap := width
	if wrap < 40 {
		wrap = 40
	}
	if wrap > 120 {
		wrap = 120
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(metisCodeBlockStyle()),
		glamour.WithWordWrap(wrap),
	)
	if err != nil {
		return nil
	}
	mdRenderer = r
	mdRendererForWidth = width
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
	r := getMarkdownRenderer(width - 4)
	if r == nil {
		return styleAsst.Render(content)
	}
	out, err := r.Render(content)
	if err != nil {
		return styleAsst.Render(content)
	}
	return strings.TrimSpace(out)
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
	return false
}
