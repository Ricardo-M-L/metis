package uv

import (
	"bytes"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestFullscreenVerticalMovesUseAbsolutePosition(t *testing.T) {
	tests := []struct {
		name  string
		fromX int
		fromY int
		toX   int
		toY   int
	}{
		{name: "down", fromX: 0, fromY: 1, toX: 0, toY: 2},
		{name: "up", fromX: 0, fromY: 2, toX: 0, toY: 1},
		{name: "down and across", fromX: 7, fromY: 1, toX: 3, toY: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			renderer := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
			renderer.SetFullscreen(true)
			renderer.SetRelativeCursor(false)
			renderer.SetScrollOptim(false)
			// A real Bubble Tea PTY enables tab stops. Without them the old
			// optimizer often selected CUP by accident and hid the LF/RI bug.
			renderer.SetTabStops(80)
			renderer.SetPosition(tt.fromX, tt.fromY)

			renderer.MoveTo(tt.toX, tt.toY)
			if err := renderer.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}

			want := ansi.CursorPosition(tt.toX+1, tt.toY+1)
			if got := out.String(); got != want {
				t.Fatalf("vertical move output = %q, want absolute CUP %q", got, want)
			}
		})
	}
}
