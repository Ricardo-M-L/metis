package tui

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestInputSelectionSurfaceMatchesRenderedCells(t *testing.T) {
	rng := rand.New(rand.NewSource(20260813))
	atoms := []string{"a", "b", " ", "你", "🙂", "e\u0301", "👩‍💻"}
	for sample := 0; sample < 200; sample++ {
		var value strings.Builder
		for i := 0; i < 5+rng.Intn(100); i++ {
			if i > 0 && rng.Intn(31) == 0 {
				value.WriteByte('\n')
				continue
			}
			value.WriteString(atoms[rng.Intn(len(atoms))])
		}

		outerWidth := 20 + rng.Intn(20)
		m := newE2EModel(t, outerWidth+4, 30, 0)
		m.input = newComposer()
		m.input.SetWidth(outerWidth)
		m.input.SetValue(value.String())

		visible := strings.Split(strings.TrimRight(ansi.Strip(m.input.View()), "\n"), "\n")
		offset := m.input.ScrollYOffset()
		m.rebuildInputSelectionSurface()
		for visibleRow, line := range visible {
			globalRow := offset + visibleRow
			if globalRow < 0 || globalRow >= len(m.inputSurface.rows) {
				continue
			}
			for _, token := range m.inputSurface.rows[globalRow].tokens {
				if token.end.displayCol > m.input.Width() {
					t.Fatalf("sample %d token crosses textarea width: token=%+v width=%d", sample, token, m.input.Width())
				}
				got := ansi.Cut(line, 2+token.start.displayCol, 2+token.end.displayCol)
				// A ZWJ is a zero-width join instruction. ANSI cell slicing may
				// retain or omit it at a soft-wrap boundary; either form refers
				// to the same painted cells and source grapheme.
				got = strings.ReplaceAll(got, "\u200d", "")
				want := strings.ReplaceAll(token.displayText, "\u200d", "")
				if got != want {
					t.Fatalf("sample %d token/view mismatch: got=%q want=%q token=%+v line=%q value=%q",
						sample, got, want, token, line, value.String())
				}
			}
		}
	}
}
