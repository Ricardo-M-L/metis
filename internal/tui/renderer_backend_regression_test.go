package tui

import (
	"bytes"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// TestRendererBackend_ClearTouchesEveryRow protects the two-frame invariant
// that Bubble Tea relies on when a tall welcome frame is replaced by a shorter
// active-chat frame. Ultraviolet versions before c5f3028e cleared the in-memory
// cells without marking those rows as touched, so the terminal never received
// erase operations for the old welcome card/input/status rows.
func TestRendererBackend_ClearTouchesEveryRow(t *testing.T) {
	const width, height = 138, 44

	buf := uv.NewRenderBuffer(width, height)
	for y := 0; y < height; y++ {
		buf.SetCell(0, y, &uv.Cell{Content: "x", Width: 1})
	}

	// Model the start of Bubble Tea's next frame: prior touched state has
	// already been consumed, then the render buffer is cleared before the new
	// frame is drawn.
	buf.Touched = make([]*uv.LineData, height)
	buf.Clear()

	if got := buf.TouchedLines(); got != height {
		t.Fatalf("clear touched %d/%d rows; stale terminal rows can survive the next frame", got, height)
	}
	for y, touched := range buf.Touched {
		if touched == nil {
			t.Fatalf("row %d was not touched by clear", y)
		}
		if touched.FirstCell != 0 || touched.LastCell < width {
			t.Fatalf("row %d clear touched cells [%d,%d), want [0,%d)", y, touched.FirstCell, touched.LastCell, width)
		}
	}
}

// TestRendererBackend_ReanchorsCJKRows protects the iTerm2 fallback added in
// Ultraviolet d160fe76/f5cce66a. When the terminal and renderer disagree about
// a wide glyph's width, one absolute horizontal move at line end contains the
// cursor error instead of letting it accumulate into whole-frame drift.
func TestRendererBackend_ReanchorsCJKRows(t *testing.T) {
	var out bytes.Buffer
	renderer := uv.NewTerminalRenderer(&out, []string{
		"TERM=xterm-256color",
		"TERM_PROGRAM=iTerm.app",
		"COLORTERM=truecolor",
	})
	renderer.SetFullscreen(true)
	renderer.SetGraphemeWidth(false)
	renderer.SaveCursor()
	renderer.Erase()

	screen := uv.NewScreenBuffer(10, 1)
	out.Reset()
	uv.NewStyledString("世界").Draw(screen, screen.Bounds())
	renderer.Render(screen.RenderBuffer)
	if err := renderer.Flush(); err != nil {
		t.Fatalf("flush CJK row: %v", err)
	}

	if got := strings.Count(out.String(), "\x1b[5G"); got != 1 {
		t.Fatalf("CJK row emitted %d end-of-line reanchors, want 1; output=%q", got, out.String())
	}
}

// TestRendererBackend_FullscreenVerticalMovesUseCUP pins the direct-iTerm2
// fix at the application module boundary. Disabling hard-scroll alone is not
// sufficient: Ultraviolet's ordinary cursor optimizer used bare LF/RI for
// one-row moves, which scroll at the physical bottom/top margin after even a
// one-row cursor drift and leave a complete old frame above the new frame.
func TestRendererBackend_FullscreenVerticalMovesUseCUP(t *testing.T) {
	tests := []struct {
		name       string
		fromY, toY int
	}{
		{name: "down", fromY: 1, toY: 2},
		{name: "up", fromY: 2, toY: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			renderer := uv.NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
			renderer.SetFullscreen(true)
			renderer.SetRelativeCursor(false)
			renderer.SetScrollOptim(false)
			renderer.SetTabStops(138)
			renderer.SetPosition(0, tt.fromY)

			renderer.MoveTo(0, tt.toY)
			if err := renderer.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}

			want := ansi.CursorPosition(1, tt.toY+1)
			if got := out.String(); got != want {
				t.Fatalf("vertical move output = %q, want absolute CUP %q", got, want)
			}
		})
	}
}
