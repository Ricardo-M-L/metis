package tui

import (
	"fmt"
	"strings"
	"testing"
	"unicode"

	xansi "github.com/charmbracelet/x/ansi"
)

func TestStreamingAssistantAlignmentMatchesFinal(t *testing.T) {
	for _, width := range []int{20, 40, 80} {
		for _, tc := range []struct {
			name, content string
			exactRows     bool
		}{
			{"explicit-newline", "First line\nSecond line", true},
			{"explicit-cjk-newline", "第一行\n第二行", true},
			{"long-cjk", strings.Repeat("完整中文内容继续显示。", 18) + "结尾", false},
			{"long-paragraph", strings.Repeat("Readable paragraph keeps the same text column. ", 12) + "END", false},
		} {
			t.Run(fmt.Sprintf("%d/%s", width, tc.name), func(t *testing.T) {
				item := &inProgressStreamingItem{text: tc.content}
				stream := stripANSI(item.Render(width))
				final := stripANSI(normalizeChatItemBoundary(renderMessage(Message{Role: "assistant", Content: tc.content}, width, false)))
				trimRowPadding := func(value string) string {
					rows := strings.Split(value, "\n")
					for i := range rows {
						rows[i] = strings.TrimRight(rows[i], " ")
					}
					return strings.Join(rows, "\n")
				}
				if tc.exactRows && trimRowPadding(stream) != trimRowPadding(final) {
					t.Fatalf("explicit newline shifts when streaming completes:\nstream %q\n final %q", stream, final)
				}
				for name, rendered := range map[string]string{"stream": stream, "final": final} {
					var reconstructed strings.Builder
					rows := strings.Split(rendered, "\n")
					if len(rows) < 2 {
						t.Fatalf("%s fixture must wrap or have an explicit newline: %q", name, rendered)
					}
					for index, row := range rows {
						if xansi.StringWidth(row) > width {
							t.Fatalf("%s row exceeds width %d: %q", name, width, row)
						}
						if strings.TrimSpace(row) == "" {
							continue
						}
						prefix := "    "
						if index == 0 {
							prefix = "  " + glyphBullet + " "
						}
						body, ok := strings.CutPrefix(row, prefix)
						if !ok || strings.HasPrefix(body, " ") {
							t.Fatalf("%s row %d does not start in the four-cell content column: %q", name, index, row)
						}
						reconstructed.WriteString(body)
					}
					withoutWhitespace := func(value string) string {
						return strings.Map(func(r rune) rune {
							if unicode.IsSpace(r) {
								return -1
							}
							return r
						}, value)
					}
					if withoutWhitespace(reconstructed.String()) != withoutWhitespace(tc.content) {
						t.Fatalf("%s lost body text while wrapping", name)
					}
				}
				if item.text != tc.content {
					t.Fatal("stream rendering changed the raw reply")
				}
			})
		}
	}
}

func TestStreamingAssistantNarrowWidthKeepsRowsWithinViewport(t *testing.T) {
	const content = "中文🙂e\u0301中文🙂e\u0301尾部"
	for _, width := range []int{1, 2, 3, 4, 5, 8, 12, 19} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			out := stripANSI((&inProgressStreamingItem{text: content}).Render(width))
			for _, row := range strings.Split(out, "\n") {
				if cells := xansi.StringWidth(row); cells > width {
					t.Fatalf("narrow streaming row is %d cells in viewport %d: %q", cells, width, row)
				}
			}
			// A one-column terminal cannot represent a double-width glyph.
			// Every wider case must preserve all original graphemes.
			if width > 1 {
				joined := strings.NewReplacer(" ", "", "\n", "", glyphBullet, "").Replace(out)
				if joined != content {
					t.Fatalf("narrow wrapping lost a grapheme: want %q, got %q", content, joined)
				}
			}
		})
	}
}

func TestStreamingAssistantUnknownWidthUsesFinalDefault(t *testing.T) {
	content := strings.Repeat("word ", 12)
	for _, width := range []int{0, -1} {
		stream := stripANSI((&inProgressStreamingItem{text: content}).Render(width))
		final := stripANSI(normalizeChatItemBoundary(renderMessage(Message{Role: "assistant", Content: content}, width, false)))
		if stream != final {
			t.Fatalf("width %d uses a different streaming default:\nstream %q\nfinal %q", width, stream, final)
		}
	}
}
