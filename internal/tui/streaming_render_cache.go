package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	xansi "github.com/charmbracelet/x/ansi"
)

// streamingRenderCache retains only the current live reply. Unlike historical
// renderCache entries, this snapshot is replaced as each stream delta arrives.
// It is owned by the Bubble Tea goroutine, like the live reply text itself.
type streamingRenderCache struct {
	text     string
	width    int
	style    string
	rendered string
	valid    bool
	joined   bool
	rows     *streamingRowLayout
}

// Snapshots are immutable: readingLiveRevision may retain this pointer while
// the current stream advances. The outer cache remains comparable/copyable.
type streamingRowLayout struct {
	rows        []string
	stableCount int
	tailOffset  int
	bodyWidth   int
	prefix      string
	appendSafe  bool
	ascii       bool
}

// asciiWrapSafe excludes ANSI/parser state, controls and Unicode grapheme
// lookahead. Ordinary UTF-8 uses only explicit LF boundaries; other inputs
// retain the exact original full Wrap path.
// With printable ASCII + LF, Wrap's scan only appends committed output; EOF
// flushes its pending word/spaces onto the final line. Keep two output lines
// uncommitted, including ALL their raw trailing spaces, for conservative reflow.
func asciiWrapSafe(text string) bool {
	for i := range text {
		if text[i] != '\n' && (text[i] < ' ' || text[i] > '~') {
			return false
		}
	}
	return true
}

func plainStreamingUTF8(text string) bool {
	if !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if r != '\n' && unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Find a restart point for the last two Wrap output lines. Matching source
// bytes alone is insufficient: Wrap can discard separator spaces. Verify the
// candidate against Wrap itself; ambiguous/unmatched boundaries fall back.
func streamingTailOffset(source string, lines []string, width int) (int, bool) {
	if len(lines) <= 2 || lines[len(lines)-2] == "" {
		return 0, false
	}
	first := lines[len(lines)-2]
	want := strings.Join(lines[len(lines)-2:], "\n")
	end := len(source)
	for range 8 {
		at := strings.LastIndex(source[:end], first)
		if at < 0 {
			break
		}
		if xansi.Wrap(source[at:], width, "") == want {
			return at, true
		}
		end = at + len(first) - 1
	}
	return 0, false
}

func (c *streamingRenderCache) replyRows(text string, width int, style string, bodyWidth int, prefix string) []string {
	previous := c.rows
	start, stableCount := 0, 0
	asciiSafe, plainSafe := false, false
	if c.valid && previous != nil && previous.appendSafe && c.width == width && c.style == style &&
		previous.bodyWidth == bodyWidth && previous.prefix == prefix && strings.HasPrefix(text, c.text) {
		if previous.ascii {
			asciiSafe = asciiWrapSafe(text[len(c.text):])
			plainSafe = asciiSafe
		} else {
			plainSafe = plainStreamingUTF8(text[len(c.text):])
		}
		if plainSafe {
			start, stableCount = previous.tailOffset, previous.stableCount
		}
	}
	if !plainSafe {
		asciiSafe = asciiWrapSafe(text)
		plainSafe = asciiSafe || plainStreamingUTF8(text)
	}
	// If an appended chunk introduces an escape/control/Unicode sequence, the
	// full source must be reprocessed with its original parser/grapheme context.
	source := text[start:]
	lines := strings.Split(xansi.Wrap(source, bodyWidth, ""), "\n")
	rows := make([]string, stableCount+len(lines))
	if stableCount > 0 {
		copy(rows, previous.rows[:stableCount])
	}
	indent := strings.Repeat(" ", xansi.StringWidth(prefix))
	for i, line := range lines {
		leader := indent
		if stableCount+i == 0 {
			leader = prefix
		}
		rows[stableCount+i] = leader + styleText.Render(clampLine(line, bodyWidth))
	}
	// Extremely narrow terminals can have an empty gutter. Match the public
	// renderer's outer-newline normalization without retaining formatting rows.
	for len(rows) > 1 && rows[0] == "" {
		rows = rows[1:]
		asciiSafe, plainSafe = false, false
	}
	for len(rows) > 1 && rows[len(rows)-1] == "" {
		rows = rows[:len(rows)-1]
		asciiSafe, plainSafe = false, false
	}
	next := &streamingRowLayout{rows: rows, bodyWidth: bodyWidth, prefix: prefix}
	if asciiSafe {
		// Even when the newly reflowed tail has only two lines, keep the
		// already-verified older boundary rather than reflowing the full reply.
		next.tailOffset, next.stableCount, next.appendSafe, next.ascii = start, stableCount, true, true
		if cut, ok := streamingTailOffset(source, lines, bodyWidth); ok {
			next.tailOffset = start + cut
			next.stableCount = stableCount + len(lines) - 2
		}
	} else if plainSafe {
		// For Unicode, only LF is a proven parser/grapheme restart boundary.
		// Keep completed paragraphs; reflow the entire current paragraph.
		if cut := strings.LastIndexByte(source, '\n') + 1; cut > 0 {
			flushed := xansi.Wrap(source[:cut], bodyWidth, "")
			next.tailOffset = start + cut
			next.stableCount = stableCount + strings.Count(flushed, "\n")
			next.appendSafe = true
		} else if start > 0 {
			next.tailOffset, next.stableCount, next.appendSafe = start, stableCount, true
		}
	}
	c.text, c.width, c.style = text, width, style
	c.rows, c.valid, c.joined, c.rendered = next, true, false, ""
	return rows
}

// The palette name alone does not identify the live reply's colors: provider
// tinting can change a palette in place. Capture the actual two styles used by
// the reply, including their ANSI color profile, without hashing reply bytes.
func streamingRenderStyleKey() string {
	return styleText.Render("stream-style") + styleAsst.Render("stream-style")
}
