package tui

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
)

func TestStreamingRenderCache_ReusesLayoutUntilInputsChange(t *testing.T) {
	var cache streamingRenderCache
	layouts := 0
	layout := func() string {
		layouts++
		return "rendered reply"
	}
	for range 4 {
		if got := cache.render("reply", 78, "dark", layout); got != "rendered reply" {
			t.Fatalf("rendered text = %q", got)
		}
	}
	if layouts != 1 {
		t.Fatalf("unchanged live reply laid out %d times, want 1", layouts)
	}
	for _, input := range []struct {
		text  string
		width int
		theme string
	}{
		{"reply appended", 78, "dark"},
		{"reply appended", 38, "dark"},
		{"reply appended", 38, "light"},
		{"replacement", 38, "light"},
	} {
		cache.render(input.text, input.width, input.theme, layout)
		cache.render(input.text, input.width, input.theme, layout)
	}
	if layouts != 5 {
		t.Fatalf("layout count after text/width/theme changes = %d, want 5", layouts)
	}
}

func TestStreamingRenderCache_MatchesFreshLayoutAfterStreamUpdates(t *testing.T) {
	var cache streamingRenderCache
	for _, text := range []string{
		"",
		" \t\n ",
		"a growing paragraph with words and punctuation.",
		strings.Repeat("完整中文🙂👩‍💻e\u0301内容继续显示。", 8),
		"first\tcolumn\tsecond\n\tindented line\ntrailing\n",
		"\x1b[31mred text\nmore red\x1b[0m normal",
		"\x1b]8;;https://example.com\x1b\\linked 👩‍💻 text\x1b]8;;\x1b\\ suffix",
		"\n- list item with a growing word and another item\n- second item",
	} {
		for _, width := range []int{0, 1, 4, 20, 78, 120} {
			item := &inProgressStreamingItem{text: text, cache: &cache}
			want := (&inProgressStreamingItem{text: text}).Render(width)
			for range 3 {
				if got := item.Render(width); got != want {
					t.Fatalf("cached rendering changed bytes for width %d, input %q:\n got %q\nwant %q", width, text, got, want)
				}
			}
			item.backgrounded = true
			if got := item.Render(width); got != "" {
				t.Fatal("backgrounded reply remained visible")
			}
			item.backgrounded = false
			if got := item.Render(width); got != want {
				t.Fatal("foregrounding lost the cached reply")
			}
		}
	}
}

func TestStreamingRenderCache_ActualStyleChangesInvalidate(t *testing.T) {
	oldText, oldAsst := styleText, styleAsst
	defer func() { styleText, styleAsst = oldText, oldAsst }()
	var cache streamingRenderCache
	item := &inProgressStreamingItem{text: "reply content", cache: &cache}
	before := item.Render(78)
	styleText = styleText.Foreground(lipgloss.Color("#123456"))
	afterText := item.Render(78)
	if afterText == before || afterText != item.render(78) {
		t.Fatal("changing actual body style reused the old live reply")
	}
	styleAsst = styleAsst.Foreground(lipgloss.Color("#654321"))
	afterLeader := item.Render(78)
	if afterLeader == afterText || afterLeader != item.render(78) {
		t.Fatal("changing actual assistant style reused the old live reply")
	}
}

func TestStreamingRenderCache_SurvivesViewSnapshotsAndClearsWhenFinished(t *testing.T) {
	m := newCachedScrollModel(80, 30)
	m.streamingText = strings.Repeat("synthetic stream ", 4096)
	m.spinnerActive = true
	m.spinnerStartedAt = time.Now()
	_ = m.View()
	if !m.streamingRenderCache.valid || m.streamingRenderCache.text != m.streamingText {
		t.Fatal("View did not populate the model's live reply cache")
	}
	first := m.streamingRenderCache
	for range 3 {
		items := m.buildChatItems()
		found := false
		for _, item := range items {
			if stream, ok := item.(*inProgressStreamingItem); ok {
				found = true
				if stream.cache != &m.streamingRenderCache {
					t.Fatal("new frame did not reuse the model's live reply cache")
				}
			}
		}
		if !found {
			t.Fatal("fixture did not produce a live reply item")
		}
		_ = m.View()
		if m.streamingRenderCache != first {
			t.Fatal("unchanged stream changed the cached snapshot between Views")
		}
	}
	m.streamingText += "appended"
	_ = m.View()
	if m.streamingRenderCache.text != m.streamingText || m.streamingRenderCache.rows == first.rows ||
		reflect.DeepEqual(m.streamingRenderCache.rows.rows, first.rows.rows) {
		t.Fatal("new stream content reused the old layout")
	}
	m.streamingText = ""
	_ = m.buildChatItems()
	if m.streamingRenderCache != (streamingRenderCache{}) {
		t.Fatal("finished stream retained its large live reply cache")
	}
}

// This fixture matches the isolated View benchmark used during the initial
// investigation: one prompt, an active spinner and a fixed-width ASCII reply.
func newStreamingBenchmarkModel(n int, thinking, top bool) *Model {
	m := newCachedScrollModel(80, 30)
	m.messages = []Message{{Role: "user", Content: "synthetic render audit", Timestamp: time.Now()}}
	text := strings.Repeat("streaming text with spaces and words. ", n/37+1)[:n]
	m.spinnerActive = true
	m.spinnerStartedAt = time.Now()
	m.firstStreamAt = time.Now()
	if thinking {
		m.thinkingText = text
	} else {
		m.streamingText = text
	}
	_ = m.View()
	if top {
		m.stickyBottom = false
		m.chatList.ScrollToTop()
	}
	return m
}

func BenchmarkStreamingView_FixedLiveTail(b *testing.B) {
	for _, n := range []int{16384, 65536, 262144} {
		b.Run(fmt.Sprintf("bytes=%d", n), func(b *testing.B) {
			m := newStreamingBenchmarkModel(n, false, false)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = m.View()
			}
		})
	}
}

func BenchmarkStreamingView_OffscreenLiveTail(b *testing.B) {
	for _, n := range []int{65536, 524288} {
		b.Run(fmt.Sprintf("bytes=%d", n), func(b *testing.B) {
			m := newStreamingBenchmarkModel(n, false, true)
			for i := range 100 {
				m.messages = append(m.messages, Message{Role: "user", Content: fmt.Sprintf("synthetic history %d", i), Timestamp: time.Now()})
			}
			_ = m.View()
			m.chatList.ScrollToTop()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = m.View()
			}
		})
	}
}

func BenchmarkStreamingView_AppendingLiveTail(b *testing.B) {
	for _, n := range []int{65536, 262144} {
		b.Run(fmt.Sprintf("bytes=%d", n), func(b *testing.B) {
			m := newStreamingBenchmarkModel(n, false, false)
			base := m.streamingText
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				m.streamingText = base + strconv.Itoa(i)
				_ = m.View()
			}
		})
	}
}

type streamingRowsItem interface {
	RenderRows(int) []string
}

func streamRowsForBenchmark(item *inProgressStreamingItem, width int) []string {
	if rows, ok := any(item).(streamingRowsItem); ok {
		return rows.RenderRows(width)
	}
	return strings.Split(strings.TrimRight(item.Render(width), "\n"), "\n")
}

func TestStreamingRenderRows_AppendParityAndImmutableSnapshots(t *testing.T) {
	streams := []string{
		strings.Repeat("A growing paragraph with short and extraordinarilylongwords. ", 12),
		strings.Repeat("long-word--with---hyphens abcdefghijklmnopqrstuvwxyz ", 12),
		strings.Repeat("-", 320) + " next words " + strings.Repeat("x", 300) + "   ",
		strings.Repeat("abc   def     ghi \n\nnext paragraph. ", 12),
		"\n\nleading empty lines followed by a longer growing paragraph of text\n\n",
		strings.Repeat("完整中文🙂👩‍💻e\u0301、连续追加而没有换行。", 12),
		strings.Repeat("完整中文🙂👩‍💻e\u0301 第一段。\n第二段继续追加，带有\u00a0空格和\u2003宽空格。\n", 8),
		"\x1b[31mred text\x1b[0m and \x1b]8;;https://example.com\x1b\\linked text\x1b]8;;\x1b\\\nnext",
		"tabs\tand\rcontrols\nnonbreaking\u00a0space\u2003wide space",
	}
	for _, width := range []int{1, 4, 12, 40, 78} {
		for _, stream := range streams {
			var cache streamingRenderCache
			var previous, snapshot []string
			for end := 1; end <= len(stream); end++ {
				item := &inProgressStreamingItem{text: stream[:end], cache: &cache}
				rowsItem, ok := any(item).(streamingRowsItem)
				if !ok {
					t.Fatal("live replies must expose cached visual rows without joining the entire reply")
				}
				rows := rowsItem.RenderRows(width)
				want := strings.Split(strings.TrimRight(item.render(width), "\n"), "\n")
				if strings.TrimSpace(item.text) == "" {
					want = []string{""}
				}
				if !reflect.DeepEqual(rows, want) {
					t.Fatalf("append rows differ at width=%d bytes=%d source=%q\n got %q\nwant %q", width, end, item.text, rows, want)
				}
				if previous != nil && !reflect.DeepEqual(previous, snapshot) {
					t.Fatal("updating a live reply mutated an earlier visual-row snapshot")
				}
				if got := item.Render(width); strings.TrimSpace(item.text) != "" && got != item.render(width) {
					t.Fatalf("public Render bytes differ at width=%d source=%q", width, item.text)
				}
				previous, snapshot = rows, append([]string(nil), rows...)
			}
		}
	}
}

func TestStreamingRenderRows_UTF8LFKeepsCompletedParagraphs(t *testing.T) {
	var cache streamingRenderCache
	base := strings.Repeat("已完成的中文段落🙂，保留原始列宽与换行。\n", 80)
	item := &inProgressStreamingItem{text: base, cache: &cache}
	first := item.RenderRows(78)
	if !cache.rows.appendSafe || cache.rows.ascii || cache.rows.tailOffset != len(base) {
		t.Fatal("ordinary UTF-8 paragraphs did not establish an explicit-LF restart boundary")
	}
	for _, chunk := range []string{"下一段", "👩", "\u200d", "💻", "e", "\u0301", "\u00a0空格", "\u2003宽空格", "\n", "最后一段"} {
		item.text += chunk
		rows := item.RenderRows(78)
		want := strings.Split(strings.TrimRight(item.render(78), "\n"), "\n")
		if !reflect.DeepEqual(rows, want) {
			t.Fatalf("LF append layout differs for chunk %q", chunk)
		}
		if cache.rows.tailOffset < len(base) || rows[0] != first[0] {
			t.Fatal("appending to the active paragraph discarded completed Unicode paragraphs")
		}
	}
}

// Unlike base+strconv.Itoa(i), each measured update really appends to the
// previous source. Periodic resets bound the fixture's memory and stream size.
func BenchmarkStreamingRows_AppendParagraph(b *testing.B) {
	for _, n := range []int{65536, 262144} {
		b.Run(fmt.Sprintf("bytes=%d", n), func(b *testing.B) {
			base := strings.Repeat("streaming paragraph with ordinary words and punctuation. ", n/55+1)[:n]
			var cache streamingRenderCache
			text := base
			item := &inProgressStreamingItem{text: text, cache: &cache}
			_ = streamRowsForBenchmark(item, 78)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				if i > 0 && i%128 == 0 {
					b.StopTimer()
					cache, text = streamingRenderCache{}, base
					item.text = text
					_ = streamRowsForBenchmark(item, 78)
					b.StartTimer()
				}
				text += " appended words."
				item.text = text
				_ = streamRowsForBenchmark(item, 78)
			}
		})
	}
}

// Full pre-incremental layout is kept only as a byte-for-byte test oracle.
func (it *inProgressStreamingItem) render(width int) string {
	// Streaming text arrives before the final assistant message goes through
	// glamour, so a provider may give us one very long paragraph with no
	// newlines. Wrap it here using the same width budget as historical
	// assistant rows. Otherwise the list counts it as one row while the
	// terminal paints/truncates it at the right edge, which desynchronizes
	// frame geometry during long, frequently repainted conversations.
	bodyW := assistantBodyWidth(width)
	prefix := assistantReplyPrefix(it.text, true)
	if width > 0 && width < 6 {
		// A normal four-cell gutter would leave no room for CJK/emoji.
		// Narrow only the decoration; keep every body row in the same column.
		prefix = xansi.Truncate(prefix, max(0, width-2), "")
		bodyW = max(1, width-xansi.StringWidth(prefix))
	}
	indent := strings.Repeat(" ", xansi.StringWidth(prefix))
	wrapped := xansi.Wrap(it.text, bodyW, "")

	var sb strings.Builder
	sb.WriteString(prefix)
	lines := strings.Split(wrapped, "\n")
	if len(lines) > 0 {
		sb.WriteString(styleText.Render(clampLine(lines[0], bodyW)))
		for _, ln := range lines[1:] {
			sb.WriteString("\n" + indent)
			sb.WriteString(styleText.Render(clampLine(ln, bodyW)))
		}
	}
	sb.WriteString("\n")
	return normalizeChatItemBoundary(sb.String())
}

// Compatibility helper for existing cache lifecycle fixtures.
func (c *streamingRenderCache) render(text string, width int, style string, layout func() string) string {
	if c.valid && c.joined && c.text == text && c.width == width && c.style == style {
		return c.rendered
	}
	c.text = text
	c.width = width
	c.style = style
	c.rendered = layout()
	c.valid = true
	c.joined = true
	return c.rendered
}
