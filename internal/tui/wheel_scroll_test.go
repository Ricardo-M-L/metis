package tui

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestWheelBatchScrollsActiveScreenWithoutMovingChat(t *testing.T) {
	m := newE2EModel(t, 100, 20, 40)
	_ = m.View()
	m.chatList.ScrollToBottom()
	var body strings.Builder
	for n := range 40 {
		fmt.Fprintf(&body, "detail row %02d\n", n)
	}
	m.openBodyScreen("/context", body.String())
	m.Update(tea.MouseWheelBatchMsg{MouseWheelMsg: tea.MouseWheelMsg{Button: tea.MouseWheelDown}, Count: 3})
	view := m.activeScreen.View()
	if strings.Contains(view, "detail row 02") || !strings.Contains(view, "detail row 03") || !m.chatList.AtBottom() {
		t.Fatalf("batch did not scroll only the active screen: %q", view)
	}
}

type wheelFrame struct {
	processed int
	at        time.Time
	atBottom  bool
}

// Observe through the decoder and event loop. Model state remains confined to
// the Bubble Tea goroutine, and timing is reported without a CI speed gate.
type wheelBurstModel struct {
	model     *Model
	base      string
	processed int
	drawn     int
	frames    chan wheelFrame
}

func (m *wheelBurstModel) Init() tea.Cmd { return nil }

func (m *wheelBurstModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch event := msg.(type) {
	case tea.MouseWheelMsg:
		m.processed++
	case tea.MouseWheelBatchMsg:
		m.processed += event.Count
	}
	if m.base != "" && m.processed > 1 && m.processed != m.drawn {
		m.model.streamingText = m.base + strconv.Itoa(m.processed)
	}
	_, cmd := m.model.Update(msg)
	return m, cmd
}

func (m *wheelBurstModel) View() tea.View {
	view := m.model.View()
	if m.processed != m.drawn {
		m.drawn = m.processed
		m.frames <- wheelFrame{processed: m.drawn, at: time.Now(), atBottom: m.model.chatList.AtBottom()}
	}
	return view
}

func TestTerminalWheelBurstCoalescesDuringStreaming(t *testing.T) {
	for _, scenario := range []struct {
		name string
		size int
	}{
		{name: "idle"},
		{name: "live64KiB", size: 64 * 1024},
		{name: "live256KiB", size: 256 * 1024},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			const width, height, reports = 120, 42, 64
			m := newE2EModel(t, width, height, 220)
			m.chatList.SetGap(chatItemGap)
			const liveLine = "Synthetic streaming content with enough words to wrap.\n"
			m.turnActive = scenario.size > 0
			m.streamingText = strings.Repeat(liveLine, scenario.size/len(liveLine))
			_ = m.View()
			m.chatList.ScrollToBottom()

			probe := &wheelBurstModel{model: m, base: m.streamingText, frames: make(chan wheelFrame, reports+1)}
			reader, writer := io.Pipe()
			out := &lockedOutput{}
			ctx, cancel := context.WithCancel(context.Background())
			p := tea.NewProgram(probe, tea.WithContext(ctx), tea.WithInput(reader), tea.WithOutput(out),
				tea.WithEnvironment([]string{"TERM=xterm-256color", "TERM_PROGRAM=iTerm.app", "COLORTERM=truecolor"}),
				tea.WithoutSignals(), tea.WithWindowSize(width, height), tea.WithMouseWheelCoalescing())
			done := make(chan error, 1)
			go func() { _, err := p.Run(); done <- err }()
			t.Cleanup(func() {
				defer cancel()
				_ = writer.Close()
				p.Quit()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(rendererTestTimeout):
					p.Kill()
					t.Error("terminal program did not stop")
				}
			})
			waitForRendererOutput(t, out, rendererTestTimeout, func(s string) bool {
				return strings.Contains(s, ansi.SetModeMouseExtSgr)
			})
			nextFrame := func() wheelFrame {
				select {
				case frame := <-probe.frames:
					return frame
				case <-time.After(rendererTestTimeout):
					t.Fatal("missing scroll frame")
					return wheelFrame{}
				}
			}
			const wheelUp = "\x1b[<64;10;10M"
			if _, err := io.WriteString(writer, wheelUp); err != nil {
				t.Fatal(err)
			}
			if nextFrame().processed != 1 {
				t.Fatal("the first wheel event did not get its own update")
			}
			if _, err := io.WriteString(writer, strings.Repeat(wheelUp, reports)); err != nil {
				t.Fatal(err)
			}
			inputDone := time.Now()
			var frames int
			var last wheelFrame
			for last.processed != reports+1 && frames < reports {
				last = nextFrame()
				frames++
			}
			if last.processed != reports+1 || frames >= reports {
				t.Fatalf("%d wheel reports yielded %d frames with %d reports processed", reports, frames, last.processed-1)
			}
			if last.atBottom {
				t.Fatal("wheel-up burst left the chat following the live tail")
			}
			t.Logf("%d terminal wheel reports, %d layout passes, final layout %s after the burst", reports, frames, last.at.Sub(inputDone).Round(time.Millisecond))
		})
	}
}
