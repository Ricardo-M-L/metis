package tea

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestCursedRendererClearRepaintsAnAlreadyFlushedView(t *testing.T) {
	for _, preflushIdle := range []bool{false, true} {
		name := "unchanged-view"
		if preflushIdle {
			name = "idle-before-clear-bookend"
		}
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			r := newCursedRenderer(&output, []string{"TERM=xterm-256color"}, 80, 24)
			view := View{Content: "final-idle-marker", AltScreen: true, MouseMode: MouseModeCellMotion}
			if preflushIdle {
				r.render(View{Content: "connecting", AltScreen: true, MouseMode: MouseModeCellMotion})
				if err := r.flush(false); err != nil {
					t.Fatal(err)
				}
			}
			r.render(view)
			if err := r.flush(false); err != nil {
				t.Fatal(err)
			}
			// Record the boundary before the explicit clear can reach output.
			offset := output.Len()
			r.clearScreen()
			if preflushIdle {
				r.clearScreen() // both asynchronous bookends may arrive after idle
			}
			if err := r.flush(false); err != nil {
				t.Fatal(err)
			}
			suffix := output.String()[offset:]
			if !strings.Contains(suffix, ansi.EraseEntireScreen) || !strings.Contains(suffix, "final-idle-marker") {
				t.Fatalf("explicit clear did not repaint the unchanged view: %q", suffix)
			}
			if strings.Contains(suffix, ansi.SetModeAltScreenSaveCursor) {
				t.Fatal("repaint incorrectly re-entered alt screen")
			}
			after := output.Len()
			if err := r.flush(false); err != nil {
				t.Fatal(err)
			}
			if output.Len() != after {
				t.Fatal("completed redraw remained pending on an ordinary unchanged flush")
			}
		})
	}
}

// A failed write can consume the lower renderer's clear flag before bytes reach
// the terminal. The next flush must still repaint the complete unchanged view.
func TestCursedRendererRetriesClearAfterOutputFailure(t *testing.T) {
	output := &redrawFailOnceWriter{}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color"}, 80, 24)
	r.render(View{Content: "retry-redraw-marker", AltScreen: true})
	if err := r.flush(false); err != nil {
		t.Fatal(err)
	}
	offset := output.Len()
	r.clearScreen()
	output.fail = true
	if err := r.flush(false); err == nil {
		t.Fatal("fixture writer unexpectedly accepted the failed flush")
	}
	if err := r.flush(false); err != nil {
		t.Fatal(err)
	}
	suffix := output.String()[offset:]
	if !strings.Contains(suffix, ansi.EraseEntireScreen) || !strings.Contains(suffix, "retry-redraw-marker") {
		t.Fatalf("retry did not repaint the complete unchanged view: %q", suffix)
	}
}

type redrawFailOnceWriter struct {
	bytes.Buffer
	fail bool
}

func (w *redrawFailOnceWriter) Write(p []byte) (int, error) {
	if w.fail {
		w.fail = false
		return 0, io.ErrClosedPipe
	}
	return w.Buffer.Write(p)
}
