package tui

// markdown_table.go — pre-render markdown tables before they reach
// glamour. Why: glamour's ANSI table renderer (charm.land/glamour/v2,
// ansi/table.go) hard-codes BorderTop/Left/Right/Bottom = false, which
// gives the "no outer frame, no header band" look in image #3 — fine
// for prose but visually thin compared to Claude Code's tables
// (image #4) which have a full ASCII frame and a grey header row.
//
// We can't hook glamour's table renderer, so instead we splice the
// markdown stream: split it into text segments and table segments,
// render text through glamour as before, render tables ourselves with
// lipgloss/v2/table (ASCIIBorder, full outer frame, headers on a grey
// band), then concatenate. This keeps glamour's prose styling intact
// while giving tables the Claude-Code look.

import (
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	ltable "charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/ansi"
)

// segmentKind tags a markdown chunk as either prose (goes to glamour)
// or a table (goes to renderMetisTable).
type segmentKind int

const (
	segText segmentKind = iota
	segTable
)

type mdSegment struct {
	kind segmentKind
	// For text segments: the raw markdown.
	// For table segments: the parsed headers/rows already extracted.
	text    string
	headers []string
	rows    [][]string
}

// splitMarkdownTables walks the markdown line by line and returns a
// list of segments. Tables are detected by the GFM shape:
//   - a header line containing at least one `|`
//   - immediately followed by a separator line that contains only
//     `|`, `-`, `:` and whitespace (and at least one `|`)
//   - then zero or more body rows containing `|`
//
// Returns ([]mdSegment, true) when at least one table was found;
// ([]mdSegment{{kind:segText, text:s}}, false) otherwise so the caller
// can take the fast path.
func splitMarkdownTables(s string) ([]mdSegment, bool) {
	lines := strings.Split(s, "\n")
	var segs []mdSegment
	var buf []string
	flushText := func() {
		if len(buf) > 0 {
			segs = append(segs, mdSegment{kind: segText, text: strings.Join(buf, "\n")})
			buf = buf[:0]
		}
	}

	i := 0
	found := false
	for i < len(lines) {
		// Try to start a table at line i: line i must contain `|` and
		// line i+1 must be a separator row.
		if i+1 < len(lines) && strings.Contains(lines[i], "|") && isTableSeparator(lines[i+1]) {
			// Collect header + body rows.
			header := splitTableRow(lines[i])
			i += 2 // skip header + separator
			var rows [][]string
			for i < len(lines) && strings.Contains(lines[i], "|") && !isTableSeparator(lines[i]) {
				row := splitTableRow(lines[i])
				// Normalize row width to header width — short rows get
				// padded with "", long rows get truncated.
				row = normalizeRow(row, len(header))
				rows = append(rows, row)
				i++
			}
			flushText()
			segs = append(segs, mdSegment{kind: segTable, headers: header, rows: rows})
			found = true
			continue
		}
		buf = append(buf, lines[i])
		i++
	}
	flushText()
	if !found {
		return []mdSegment{{kind: segText, text: s}}, false
	}
	return segs, true
}

// isTableSeparator returns true for lines like `|---|---|`,
// `|:---:|---:|`, `| :-- | --: |`. After stripping `|`, `:`, `-`, and
// whitespace, nothing must remain; and the line must have at least
// one `|` plus at least one `-`.
func isTableSeparator(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.Contains(trimmed, "|") || !strings.Contains(trimmed, "-") {
		return false
	}
	for _, r := range trimmed {
		switch r {
		case '|', '-', ':', ' ', '\t':
		default:
			return false
		}
	}
	return true
}

// splitTableRow splits `| a | b | c |` into ["a", "b", "c"]. Trims the
// leading and trailing `|` and strips surrounding whitespace from each
// cell. Treats backslash-escaped `\|` inside a cell as a literal pipe.
func splitTableRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	// Split on unescaped `|`. Simple state machine, no regex.
	var cells []string
	var cur strings.Builder
	escaped := false
	for _, r := range line {
		if escaped {
			cur.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '|' {
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	cells = append(cells, strings.TrimSpace(cur.String()))
	return cells
}

func normalizeRow(row []string, n int) []string {
	if len(row) == n {
		return row
	}
	if len(row) > n {
		return row[:n]
	}
	out := make([]string, n)
	copy(out, row)
	return out
}

// renderMetisTable renders a markdown table using lipgloss/v2/table
// with a Claude-Code-style look: full Unicode outer frame + per-row
// dividers, header row on a dim grey band, body rows keep whatever
// ANSI styling came in via inline markdown (bold/italic/code spans).
// The output is sized to fit within `width` cells so it never wraps
// in the terminal.
func renderMetisTable(headers []string, rows [][]string, width int) string {
	if width < 20 {
		width = 20
	}
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#e0e0e0")).
		Background(lipgloss.Color("#3a3a3a")).
		Padding(0, 1)
	// Body cells: DO NOT set Foreground. The cells already contain
	// ANSI from renderInlineMD (red code spans, bold/italic), and a
	// Foreground here would override and flatten them to a single grey
	// (image #6 user feedback 2026-05-15 — everything looked grey).
	// Padding only for breathing room next to the │ separators.
	cellStyle := lipgloss.NewStyle().Padding(0, 1)

	// Pre-render inline markdown in each cell so `code`, **bold** and
	// *italic* show up as ANSI rather than literal `*` and backticks.
	renderedHeaders := make([]string, len(headers))
	for i, h := range headers {
		renderedHeaders[i] = renderInlineMD(h)
	}
	renderedRows := make([][]string, len(rows))
	for i, row := range rows {
		rr := make([]string, len(row))
		for j, cell := range row {
			rr[j] = renderInlineMD(cell)
		}
		renderedRows[i] = rr
	}

	// Shrink to content width when content fits within the terminal —
	// image #11 user feedback: a 3-column table with short cells was
	// stretching to fill the whole 200-col window, which looked airy
	// and empty. Estimate the natural width (longest cell per column +
	// padding + column dividers + outer frame) and use min(natural,
	// terminal). When natural > terminal, keep the terminal cap so
	// lipgloss wraps cells instead of overflowing.
	useWidth := width
	if nat := naturalTableWidth(renderedHeaders, renderedRows); nat > 0 && nat < useWidth {
		useWidth = nat
	}

	// NormalBorder gives the ─│┼┌┐└┘├┤┬┴ box-drawing glyphs that match
	// image #5 (Claude Code's table look). lipgloss internally measures
	// width with ansi.StringWidth (ambiguous-width = 1), which matches
	// how macOS Terminal / iTerm / VS Code in en_US locale render
	// these glyphs — so the visual frame and the column arithmetic
	// stay in sync, no overflow. On a true CJK-locale terminal where
	// ambiguous-width = 2, the same table will visibly double in width
	// and wrap; if that ever becomes an issue, switch back to
	// lipgloss.ASCIIBorder() in this one line.
	//
	// BorderRow(false): per-row dividers (├─┤) make the table look
	// "layered" / shadowed (image #6 user feedback). Claude Code's
	// tables only divide header from body, not body rows — match that.
	//
	// BorderStyle uses the terminal's default foreground (no Foreground
	// override) rather than a dim grey: image #8 user feedback showed
	// that #a0a0a0 was too faint, the body's │ separators disappeared
	// in their terminal and the header looked like a floating "shadow
	// card" detached from the body. Using the default fg keeps the
	// frame the same brightness as the cell text, which is what makes
	// image #5 (Claude Code) feel like one cohesive box.
	// Full grid: ┌┐└┘ corners, │ on every column boundary, ─ on every
	// row boundary including the header divider. Image #10 user
	// feedback called the previous "frame + header band only" look
	// not 完善 / not a "real" table — and confirmed that the apparent
	// shadow under the header in image #9 wasn't from BorderHeader
	// being on, it was from body rows lacking horizontal dividers so
	// the header sat alone above an unframed blob. With both
	// BorderHeader and BorderRow on, every cell is enclosed, no row
	// stands out, no shadow.
	t := ltable.New().
		Border(lipgloss.NormalBorder()).
		BorderTop(true).
		BorderBottom(true).
		BorderLeft(true).
		BorderRight(true).
		BorderHeader(true).
		BorderColumn(true).
		BorderRow(true).
		Width(useWidth).
		Wrap(true).
		Headers(renderedHeaders...).
		Rows(renderedRows...).
		StyleFunc(func(row, _ int) lipgloss.Style {
			if row == ltable.HeaderRow {
				return headerStyle
			}
			return cellStyle
		})
	return t.String()
}

// renderInlineMD turns the inline-only markdown that can appear inside
// a table cell into ANSI: **bold**, *italic* / _italic_, and `code`
// spans. Block-level constructs (lists, headings, fences) never appear
// in GFM table cells so we don't handle them. Anything that doesn't
// match a recognised opener passes through verbatim, including stray
// `*` and backticks that aren't part of a pair.
//
// Code spans get *heuristic syntax-highlighting* via classifyCodeSpan
// — Claude Code's TUI uses a single colour for all backticks (image #7
// shows that the multi-colour look comes from a markdown viewer, not
// from the terminal), so for metis we go beyond Claude Code and tint
// constants / function-calls / namespaced paths / numbers each in
// their own Dracula-palette colour. Makes table cells noticeably
// easier to scan.
//
// lipgloss measures cell width via ansi.StringWidth, which strips ANSI
// escapes before counting, so the column arithmetic still works after
// this transformation. Without this, a cell like "`Fork()` 继承父上下
// 文" rendered through lipgloss table verbatim: backticks were shown
// as raw characters and there was no coloured code badge (image #6
// user feedback).
func renderInlineMD(s string) string {
	boldStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffffff"))
	italicStyle := lipgloss.NewStyle().Italic(true).Foreground(lipgloss.Color("#bd93f9"))

	var sb strings.Builder
	runes := []rune(s)
	i := 0
	for i < len(runes) {
		// `code`
		if runes[i] == '`' {
			if end := indexRune(runes, i+1, '`'); end > 0 {
				text := string(runes[i+1 : end])
				sb.WriteString(classifyCodeSpan(text).Render(text))
				i = end + 1
				continue
			}
		}
		// **bold** — must come before *italic*
		if i+1 < len(runes) && runes[i] == '*' && runes[i+1] == '*' {
			if end := indexDoubleStar(runes, i+2); end > 0 {
				sb.WriteString(boldStyle.Render(string(runes[i+2 : end])))
				i = end + 2
				continue
			}
		}
		// *italic* / _italic_
		if (runes[i] == '*' || runes[i] == '_') &&
			(i == 0 || runes[i-1] == ' ' || runes[i-1] == '\t') {
			if end := indexRune(runes, i+1, runes[i]); end > 0 {
				sb.WriteString(italicStyle.Render(string(runes[i+1 : end])))
				i = end + 1
				continue
			}
		}
		sb.WriteRune(runes[i])
		i++
	}
	return sb.String()
}

// Two-colour inline-code palette (image #8 user feedback: 5 colours
// were too noisy, restrict to 2-3). Heuristic: digits-and-units get a
// distinct cyan so eyes can quickly find sizes / durations / status
// codes (800ms, 64k, 5xx); everything else — identifiers, function
// calls, paths, namespaces, constants, ordinary words — gets the
// orange code badge. Bold / italic are separate text styles and don't
// count towards this colour budget.
var (
	codeStyleCode   = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffb86c")) // all non-numeric code spans (Dracula orange)
	codeStyleNumber = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd")) // 800ms, 64k, 5xx (Dracula cyan)

	// Number: digit-led, optional fractional, optional unit (ms/s/k/m/g/B/x).
	// Examples that hit: 800ms, 5xx, 64k, 1.5s, 200, 1e6
	reNumberish = regexp.MustCompile(`^-?\d+(\.\d+)?[a-zA-Z%]*$|^\d+x{1,2}$|^\d+e\d+$`)
)

// classifyCodeSpan picks a colour for `code` based on the token shape.
// Numbers + units get cyan; everything else gets orange. Anything more
// fine-grained than this read as noise (user feedback on image #8).
func classifyCodeSpan(text string) lipgloss.Style {
	t := strings.TrimSpace(text)
	if reNumberish.MatchString(t) {
		return codeStyleNumber
	}
	return codeStyleCode
}

func indexRune(runes []rune, from int, r rune) int {
	for i := from; i < len(runes); i++ {
		if runes[i] == r {
			return i
		}
	}
	return -1
}

func indexDoubleStar(runes []rune, from int) int {
	for i := from; i+1 < len(runes); i++ {
		if runes[i] == '*' && runes[i+1] == '*' {
			return i
		}
	}
	return -1
}

// naturalTableWidth estimates the width a content-sized table would
// occupy if lipgloss let cells take their longest single-line content.
// Layout we mirror:
//
//	│ <pad> longest-cell-of-col-0 <pad> │ ... │ <pad> longest-cell-of-col-N <pad> │
//
// Each cell contributes its widest line plus 2 cells of padding (the
// `Padding(0, 1)` in headerStyle/cellStyle), and each column boundary
// plus the outer left+right frame contributes one cell of vertical
// border. ansi.StringWidth strips SGR codes before counting so the
// already-coloured inline-code badges don't inflate the estimate.
func naturalTableWidth(headers []string, rows [][]string) int {
	cols := len(headers)
	if cols == 0 {
		return 0
	}
	colMax := make([]int, cols)
	consider := func(j int, cell string) {
		if j < 0 || j >= cols {
			return
		}
		for _, line := range strings.Split(cell, "\n") {
			if w := ansi.StringWidth(line); w > colMax[j] {
				colMax[j] = w
			}
		}
	}
	for j, h := range headers {
		consider(j, h)
	}
	for _, row := range rows {
		for j, cell := range row {
			consider(j, cell)
		}
	}
	total := 0
	for _, w := range colMax {
		total += w + 2 // 2 padding cells per column (1 left + 1 right)
	}
	total += cols + 1 // (cols-1) column separators + 1 left frame + 1 right frame
	return total
}
