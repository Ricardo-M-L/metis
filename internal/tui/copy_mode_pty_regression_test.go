package tui

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestCopyModeRendererExitsAltScreenBeforeFullTranscript protects the exact
// byte ordering required by terminal scrollback. A direct fmt.Println from
// Update writes while DECSET 1049 is still active and loses the top of long
// transcripts. The normal-screen restore must precede every transcript row.
func TestCopyModeRendererExitsAltScreenBeforeFullTranscript(t *testing.T) {
	const (
		width     = 145
		height    = 44
		lineCount = 128
	)

	m := newE2EModel(t, width, height, 0)
	var content strings.Builder
	for i := 0; i < lineCount; i++ {
		fmt.Fprintf(&content, "copy-transcript-line-%03d\n", i)
	}
	m.messages = append(m.messages, Message{
		Role:    "assistant",
		Content: strings.TrimSuffix(content.String(), "\n"),
	})

	p, out := startTransitionRendererAtSize(t, m, width, height)
	waitForRendererOutput(t, out, rendererTestTimeout, func(s string) bool {
		return strings.Contains(s, ansi.SetModeAltScreenSaveCursor)
	})
	baseline := len(out.snapshot())

	p.Send(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	got := waitForRendererOutput(t, out, rendererTestTimeout, func(s string) bool {
		if len(s) <= baseline {
			return false
		}
		suffix := s[baseline:]
		return strings.Contains(suffix, ansi.ResetModeAltScreenSaveCursor) &&
			strings.Contains(suffix, "copy-transcript-line-000") &&
			strings.Contains(suffix, "copy-transcript-line-127")
	})

	suffix := got[baseline:]
	exitAt := strings.Index(suffix, ansi.ResetModeAltScreenSaveCursor)
	firstAt := strings.Index(suffix, "copy-transcript-line-000")
	if exitAt < 0 || firstAt < 0 || exitAt > firstAt {
		t.Fatalf("copy transcript was emitted before alt-screen exit: exit=%d first=%d output=%q",
			exitAt, firstAt, tailForRendererTest(suffix, 1000))
	}
	afterExit := suffix[exitAt+len(ansi.ResetModeAltScreenSaveCursor):]
	for i := 0; i < lineCount; i++ {
		marker := fmt.Sprintf("copy-transcript-line-%03d", i)
		if !strings.Contains(afterExit, marker) {
			t.Fatalf("normal-screen transcript missing %q", marker)
		}
	}
	lastAt := strings.Index(afterExit, "copy-transcript-line-127")
	hintAt := strings.Index(afterExit, copyModeHint)
	if hintAt < lastAt {
		t.Fatalf("copy-mode return hint must follow the transcript: last=%d hint=%d",
			lastAt, hintAt)
	}
}

type wheelProgramResult struct {
	before float64
	after  float64
}

// wheelProgramModel observes the real decoder -> Program -> Metis Update path
// without reading Model state concurrently from the test goroutine.
type wheelProgramModel struct {
	model   *Model
	results chan wheelProgramResult
}

func (m *wheelProgramModel) Init() tea.Cmd { return nil }

func (m *wheelProgramModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	before := m.model.chatList.ScrollPercent()
	_, cmd := m.model.Update(msg)
	if _, ok := msg.(tea.MouseWheelMsg); ok {
		m.results <- wheelProgramResult{
			before: before,
			after:  m.model.chatList.ScrollPercent(),
		}
	}
	return m, cmd
}

func (m *wheelProgramModel) View() tea.View { return m.model.View() }

// TestRealSGRWheelScrollsLongTranscript feeds the bytes iTerm2/tmux actually
// sends instead of injecting a synthetic MouseWheelMsg. This catches the
// regression where the handler worked in isolation but View requested
// MouseModeNone, so no wheel bytes ever reached it in a real terminal.
func TestRealSGRWheelScrollsLongTranscript(t *testing.T) {
	const width, height = 145, 44

	metis := newE2EModel(t, width, height, 80)
	_ = metis.View()
	metis.chatList.ScrollToBottom()
	if !metis.chatList.AtBottom() {
		t.Fatal("setup: long transcript must begin at bottom")
	}

	probe := &wheelProgramModel{
		model:   metis,
		results: make(chan wheelProgramResult, 1),
	}
	reader, writer := io.Pipe()
	out := &lockedOutput{}
	p := tea.NewProgram(
		probe,
		tea.WithContext(t.Context()),
		tea.WithInput(reader),
		tea.WithOutput(out),
		tea.WithEnvironment([]string{
			"TERM=xterm-256color",
			"TERM_PROGRAM=iTerm.app",
			"TERM_PROGRAM_VERSION=3.6.10",
			"COLORTERM=truecolor",
		}),
		tea.WithoutSignals(),
		tea.WithWindowSize(width, height),
	)

	runDone := make(chan error, 1)
	go func() {
		_, err := p.Run()
		runDone <- err
	}()
	t.Cleanup(func() {
		_ = writer.Close()
		p.Quit()
		select {
		case <-runDone:
		case <-time.After(time.Second):
			p.Kill()
		}
	})

	// DECSET 1002 + 1006 proves the active View requested cell-motion SGR
	// reporting before we send an actual wheel-up report (button code 64).
	waitForRendererOutput(t, out, rendererTestTimeout, func(s string) bool {
		return strings.Contains(s, ansi.SetModeMouseButtonEvent) &&
			strings.Contains(s, ansi.SetModeMouseExtSgr)
	})
	if _, err := io.WriteString(writer, "\x1b[<64;10;10M"); err != nil {
		t.Fatalf("write SGR wheel input: %v", err)
	}

	select {
	case result := <-probe.results:
		if result.after >= result.before {
			t.Fatalf("real wheel-up did not move chat toward history: before=%v after=%v",
				result.before, result.after)
		}
	case <-time.After(rendererTestTimeout):
		t.Fatal("real SGR wheel bytes never reached Metis Update")
	}
}
