package tui

import (
	"bytes"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

// TestWelcomeTransitionInvariantAppliesOutsideEnter locks the redraw at the
// Update boundary rather than at one key handler. Ctrl+L is a convenient
// synchronous non-submit path that appends the first info row.
func TestWelcomeTransitionInvariantAppliesOutsideEnter(t *testing.T) {
	m := newE2EModel(t, 100, 40, 0)
	if !m.rendersWelcomeFrame() {
		t.Fatal("setup: model should begin on the welcome frame")
	}

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("non-submit welcome -> active transition did not schedule a redraw")
	}
	if m.rendersWelcomeFrame() {
		t.Fatal("Ctrl+L should append the first info row and leave welcome state")
	}

	msg := cmd()
	sequence := reflect.ValueOf(msg)
	if sequence.Kind() != reflect.Slice {
		t.Fatalf("redraw command = %T (kind=%s), want a two-command clear bookend",
			msg, sequence.Kind())
	}
	if sequence.Len() != 2 {
		t.Fatalf("redraw sequence length = %d, want 2", sequence.Len())
	}
}

func TestWelcomeTransitionInvariantPreservesCopyModeScrollback(t *testing.T) {
	m := newE2EModel(t, 100, 40, 0)

	_, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if !m.copyMode {
		t.Fatal("Ctrl+S should enter copy mode")
	}
	if cmd != nil {
		t.Fatalf("entering copy mode returned %T; a ClearScreen wrapper would erase native scrollback", cmd)
	}
}

// lockedOutput is an io.Writer whose snapshots can safely be inspected while
// Bubble Tea's renderer goroutine is still flushing terminal bytes.
type lockedOutput struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// rendererProbeMsg gives renderer-transition tests a synchronous event-loop
// turn without adding any production behavior of its own. Update's agent-event
// drain still runs before the otherwise-unhandled message falls through.
type rendererProbeMsg struct{}

const rendererTestTimeout = 5 * time.Second

func (o *lockedOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.Write(p)
}

func (o *lockedOutput) snapshot() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}

func waitForRendererOutput(t *testing.T, out *lockedOutput, timeout time.Duration, predicate func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snapshot := out.snapshot()
		if predicate(snapshot) {
			return snapshot
		}
		time.Sleep(5 * time.Millisecond)
	}
	snapshot := out.snapshot()
	t.Fatalf("timed out waiting for renderer output; tail=%q", tailForRendererTest(snapshot, 500))
	return ""
}

func tailForRendererTest(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

func startTransitionRenderer(t *testing.T, m *Model) (*tea.Program, *lockedOutput) {
	t.Helper()

	out := &lockedOutput{}
	p := tea.NewProgram(
		m,
		tea.WithContext(t.Context()),
		tea.WithInput(nil),
		tea.WithOutput(out),
		tea.WithEnvironment([]string{
			"TERM=xterm-256color",
			"TERM_PROGRAM=iTerm.app",
			"TERM_PROGRAM_VERSION=3.6.10",
			"COLORTERM=truecolor",
		}),
		tea.WithoutSignals(),
		tea.WithWindowSize(100, 40),
	)

	runDone := make(chan error, 1)
	go func() {
		_, err := p.Run()
		runDone <- err
	}()
	t.Cleanup(func() {
		p.Quit()
		select {
		case <-runDone:
		case <-time.After(time.Second):
			p.Kill()
		}
	})
	return p, out
}

func assertRendererTransitionEndsWithFullRedraw(t *testing.T, suffix, marker string) {
	t.Helper()

	lastClear := strings.LastIndex(suffix, ansi.EraseEntireScreen)
	if lastClear < 0 {
		t.Fatalf("renderer transition never forced ED2; output=%q", suffix)
	}
	if !strings.Contains(suffix[lastClear:], marker) {
		t.Fatalf("final ED2 did not repaint marker %q; output=%q",
			marker, suffix[lastClear:])
	}

	// CSI Ps L/M/S/T are insert/delete/scroll operations used by the
	// fullscreen hard-scroll optimizer. A later ED2 guarantees their result is
	// replaced by a renderer state anchored at the physical screen origin.
	hardScroll := regexp.MustCompile("\x1b\\[[0-9;]*[LMST]")
	lastHardScroll := -1
	for _, loc := range hardScroll.FindAllStringIndex(suffix, -1) {
		lastHardScroll = loc[0]
	}
	if lastClear <= lastHardScroll {
		t.Fatalf("last ED2 at byte %d did not follow hard-scroll byte %d; output=%q",
			lastClear, lastHardScroll, suffix)
	}
}

func rendererMarkerAppearsAfterClear(s string, offset, minClearCount int, marker string) bool {
	if offset > len(s) || strings.Count(s, ansi.EraseEntireScreen) < minClearCount {
		return false
	}
	suffix := s[offset:]
	lastClear := strings.LastIndex(suffix, ansi.EraseEntireScreen)
	return lastClear >= 0 && strings.Contains(suffix[lastClear:], marker)
}

func assertSingleActiveFrameAfterLastClear(t *testing.T, suffix, responseMarker string, idle bool) {
	t.Helper()

	lastClear := strings.LastIndex(suffix, ansi.EraseEntireScreen)
	if lastClear < 0 {
		t.Fatalf("active transition did not contain ED2; output=%q", suffix)
	}
	finalFrame := suffix[lastClear:]
	for _, marker := range []string{"✻ metis ", "renderer-user-marker", responseMarker} {
		if count := strings.Count(finalFrame, marker); count != 1 {
			t.Fatalf("final frame contains %q %d times, want exactly 1; output=%q",
				marker, count, finalFrame)
		}
	}
	if idle && strings.Contains(finalFrame, "connecting") {
		t.Fatalf("final idle frame retained the live connecting row; output=%q", finalFrame)
	}
}

// TestWelcomeToActiveTransitionEndsWithFullRedraw exercises the real Bubble
// Tea renderer, not just Model.View. In v0.4.6 the exact diff depended on input
// timing: batched text+Enter could trigger ED2, while human-style rune-by-rune
// input emitted hard-scroll/delete-line operations and then entered active chat
// without a final ED2. Direct iTerm2 could retain the complete welcome frame.
//
// A first submit must therefore finish with ED2 after any hard-scroll escape.
// The two-clear count also verifies both sides of the command bookend.
func TestWelcomeToActiveTransitionEndsWithFullRedraw(t *testing.T) {
	m := newSlashTestModel(t)
	m.messages = nil
	m.firstRender = true
	m.showBanner = true
	m.ctx = t.Context()
	m.eventCh = make(chan agent.Event, 16)
	m.doneCh = make(chan error, 1)

	p, out := startTransitionRenderer(t, m)

	initial := waitForRendererOutput(t, out, rendererTestTimeout, func(s string) bool {
		return strings.Contains(s, "Type a message to start")
	})
	transitionOffset := len(initial)
	initialClearCount := strings.Count(initial, ansi.EraseEntireScreen)

	const marker = "renderer-transition-marker"
	for _, r := range marker {
		p.Send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	// Send is unbuffered. Receiving Enter cannot happen until the event loop
	// has processed and rendered every preceding rune, so a separate raw-byte
	// wait for the input text is unnecessary (and invalid: a diff renderer may
	// emit all but the last rune, move the cursor, then emit that rune
	// non-contiguously).
	p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	snapshot := waitForRendererOutput(t, out, rendererTestTimeout, func(s string) bool {
		return rendererMarkerAppearsAfterClear(
			s, transitionOffset, initialClearCount+2, marker,
		)
	})
	suffix := snapshot[transitionOffset:]
	assertRendererTransitionEndsWithFullRedraw(t, suffix, marker)
}

func newActiveTransitionRendererModel(t *testing.T, phase string) *Model {
	t.Helper()

	m := newSlashTestModel(t)
	m.messages = []Message{{
		Role:      "user",
		Content:   "renderer-user-marker",
		Timestamp: time.Now(),
	}}
	m.firstRender = false
	m.showBanner = false
	m.ctx = t.Context()
	m.eventCh = make(chan agent.Event, 16)
	m.doneCh = make(chan error, 1)
	m.turnActive = true
	m.spinnerActive = true
	m.spinnerPhase = phase
	m.spinnerVerb = "connecting"
	m.spinnerStartedAt = time.Now()
	m.stickyBottom = true
	return m
}

// TestRequestingToStreamingTransitionEndsWithFullRedraw reproduces the v0.4.7
// iTerm2 failure: the requesting frame remained at the top while the first
// streamed reply caused a second header/chat frame to be drawn below it.
func TestRequestingToStreamingTransitionEndsWithFullRedraw(t *testing.T) {
	m := newActiveTransitionRendererModel(t, "requesting")
	p, out := startTransitionRenderer(t, m)

	initial := waitForRendererOutput(t, out, rendererTestTimeout, func(s string) bool {
		return strings.Contains(s, "renderer-user-marker") &&
			strings.Contains(s, "connecting")
	})
	transitionOffset := len(initial)
	initialClearCount := strings.Count(initial, ansi.EraseEntireScreen)

	const responseMarker = "renderer-first-stream-marker"
	m.eventCh <- agent.Event{Kind: agent.EventTextDelta, TextDelta: responseMarker}
	p.Send(rendererProbeMsg{})

	snapshot := waitForRendererOutput(t, out, rendererTestTimeout, func(s string) bool {
		return rendererMarkerAppearsAfterClear(
			s, transitionOffset, initialClearCount+1, responseMarker,
		)
	})
	suffix := snapshot[transitionOffset:]
	assertRendererTransitionEndsWithFullRedraw(t, suffix, responseMarker)
	assertSingleActiveFrameAfterLastClear(t, suffix, responseMarker, false)
}

func TestRequestingToStreamingTransitionSchedulesClearBookend(t *testing.T) {
	m := newActiveTransitionRendererModel(t, "requesting")
	m.eventCh <- agent.Event{
		Kind:      agent.EventTextDelta,
		TextDelta: "renderer-first-stream-marker",
	}

	_, cmd := m.Update(rendererProbeMsg{})
	if cmd == nil {
		t.Fatal("requesting -> responding transition did not schedule a redraw")
	}
	msg := cmd()
	sequence := reflect.ValueOf(msg)
	if sequence.Kind() != reflect.Slice {
		t.Fatalf("redraw command = %T (kind=%s), want a two-command clear bookend",
			msg, sequence.Kind())
	}
	if sequence.Len() != 2 {
		t.Fatalf("redraw sequence length = %d, want 2", sequence.Len())
	}
}

// TestStreamingToIdleTransitionEndsWithFullRedraw covers the other edge of
// the same geometry change. finalizeTurn moves the live row into transcript
// history and removes the spinner; a stale hard-scroll result must not survive
// underneath that final frame.
func TestStreamingToIdleTransitionEndsWithFullRedraw(t *testing.T) {
	m := newActiveTransitionRendererModel(t, "responding")
	const responseMarker = "renderer-final-response-marker"
	m.streamingText = responseMarker
	m.firstStreamAt = time.Now()

	p, out := startTransitionRenderer(t, m)
	initial := waitForRendererOutput(t, out, rendererTestTimeout, func(s string) bool {
		return strings.Contains(s, "renderer-user-marker") &&
			strings.Contains(s, responseMarker)
	})
	transitionOffset := len(initial)
	initialClearCount := strings.Count(initial, ansi.EraseEntireScreen)

	m.doneCh <- nil
	p.Send(spinnerTick{})

	snapshot := waitForRendererOutput(t, out, rendererTestTimeout, func(s string) bool {
		return rendererMarkerAppearsAfterClear(
			s, transitionOffset, initialClearCount+1, responseMarker,
		)
	})
	suffix := snapshot[transitionOffset:]
	assertRendererTransitionEndsWithFullRedraw(t, suffix, responseMarker)
	assertSingleActiveFrameAfterLastClear(t, suffix, responseMarker, true)
}

func TestActiveFrameTransitionDoesNotClearEveryTextDelta(t *testing.T) {
	m := newActiveTransitionRendererModel(t, "responding")
	m.streamingText = "first"
	m.eventCh <- agent.Event{Kind: agent.EventTextDelta, TextDelta: " second"}

	_, cmd := m.Update(rendererProbeMsg{})
	if cmd != nil {
		t.Fatalf("responding -> responding text delta returned %T; stable streaming must not clear every token", cmd)
	}
}

func TestActiveFrameTransitionPreservesCopyModeScrollback(t *testing.T) {
	m := newActiveTransitionRendererModel(t, "requesting")
	m.copyMode = true
	m.eventCh <- agent.Event{
		Kind:      agent.EventTextDelta,
		TextDelta: "copy-mode-stream-marker",
	}

	_, cmd := m.Update(rendererProbeMsg{})
	if cmd != nil {
		t.Fatalf("requesting -> responding in copy mode returned %T; native scrollback must not be cleared", cmd)
	}
}

func TestFrameGeometryReanchorMatrix(t *testing.T) {
	tests := []struct {
		name          string
		before, after frameGeometryPhase
		want          bool
	}{
		{"welcome to active", frameGeometryWelcome, frameGeometryRequesting, true},
		{"welcome to screen", frameGeometryWelcome, frameGeometryOther, true},
		{"idle to requesting", frameGeometryIdle, frameGeometryRequesting, true},
		{"requesting to live", frameGeometryRequesting, frameGeometryLive, true},
		{"live to requesting", frameGeometryLive, frameGeometryRequesting, true},
		{"live to idle", frameGeometryLive, frameGeometryIdle, true},
		{"stable live", frameGeometryLive, frameGeometryLive, false},
		{"enter copy", frameGeometryRequesting, frameGeometryCopy, false},
		{"exit copy", frameGeometryCopy, frameGeometryIdle, false},
		{"screen to active", frameGeometryOther, frameGeometryIdle, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsFrameGeometryReanchor(tt.before, tt.after); got != tt.want {
				t.Fatalf("needsFrameGeometryReanchor(%v, %v) = %v, want %v",
					tt.before, tt.after, got, tt.want)
			}
		})
	}
}
