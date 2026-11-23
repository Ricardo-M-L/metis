package tea

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

type wheelProbeModel struct {
	wheels     chan Msg
	seen       []string
	steps      int
	wheelCalls int
	batchCalls int
	views      int
	viewDelay  time.Duration
	badBatch   bool
}

func (*wheelProbeModel) Init() Cmd { return nil }

func (m *wheelProbeModel) Update(msg Msg) (Model, Cmd) {
	count := 0
	var wheel MouseWheelMsg
	switch msg := msg.(type) {
	case MouseWheelMsg:
		wheel, count = msg, 1
	case MouseWheelBatchMsg:
		wheel, count = msg.MouseWheelMsg, msg.Count
		m.batchCalls++
		if count < 1 || count > MaxMouseWheelBatchCount || (wheel.Button != MouseWheelUp && wheel.Button != MouseWheelDown) {
			m.badBatch = true
		}
	case KeyPressMsg:
		m.seen = append(m.seen, "key:"+msg.String())
		if msg.String() == "q" {
			return m, Quit
		}
	case FocusMsg:
		m.seen = append(m.seen, "focus")
	case BlurMsg:
		m.seen = append(m.seen, "blur")
	}
	if count > 0 {
		m.wheelCalls++
		if wheel.Button == MouseWheelUp {
			m.steps -= count
		} else if wheel.Button == MouseWheelDown {
			m.steps += count
		}
		for i := 0; i < count; i++ {
			m.seen = append(m.seen, fmt.Sprintf("wheel:%d:%d:%d:%d", wheel.Button, wheel.X, wheel.Y, wheel.Mod))
		}
		m.wheels <- msg
	}
	return m, nil
}

func (m *wheelProbeModel) View() View {
	m.views++
	if m.viewDelay > 0 {
		time.Sleep(m.viewDelay)
	}
	return NewView("wheel probe")
}

type wheelProbeProgram struct {
	p     *Program
	m     *wheelProbeModel
	input *io.PipeWriter
	done  chan struct{}
	err   error
}

func startWheelProbe(t *testing.T, enabled bool, delay time.Duration, extra ...ProgramOption) *wheelProbeProgram {
	t.Helper()
	reader, writer := io.Pipe()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	m := &wheelProbeModel{wheels: make(chan Msg, 512), viewDelay: delay}
	options := []ProgramOption{WithInput(reader), WithOutput(io.Discard), WithContext(ctx), WithoutRenderer(), WithoutSignalHandler()}
	if enabled {
		options = append(options, WithMouseWheelCoalescing())
	}
	options = append(options, extra...)
	probe := &wheelProbeProgram{p: NewProgram(m, options...), m: m, input: writer, done: make(chan struct{})}
	go func() {
		_, probe.err = probe.p.Run()
		close(probe.done)
	}()
	t.Cleanup(func() {
		cancel()
		_ = writer.Close()
		_ = reader.Close()
		select {
		case <-probe.done:
		case <-time.After(time.Second):
			t.Error("wheel probe did not stop after cancellation")
		}
	})
	return probe
}

func (p *wheelProbeProgram) write(t *testing.T, input string) {
	t.Helper()
	if _, err := io.WriteString(p.input, input); err != nil {
		t.Fatal(err)
	}
}

func (p *wheelProbeProgram) finish(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		t.Fatal("wheel probe did not process its quit key")
	}
	if p.err != nil {
		t.Fatal(p.err)
	}
}

func sgrWheel(button, x, y int) string {
	return fmt.Sprintf("\x1b[<%d;%d;%dM", button, x+1, y+1)
}

func TestWheelCoalescingActualInputPipeIsOptInAndAvoidsPerEventViews(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			probe := startWheelProbe(t, enabled, 2*time.Millisecond)
			probe.write(t, strings.Repeat(sgrWheel(64, 8, 4), 96)+"kq")
			probe.finish(t)
			t.Logf("decoded input=96 wheels; opt-in=%v wheel updates=%d batch updates=%d View calls=%d (2ms scripted View)", enabled, probe.m.wheelCalls, probe.m.batchCalls, probe.m.views)
			if probe.m.steps != -96 || probe.m.badBatch {
				t.Fatalf("lost or malformed decoded wheel steps: steps=%d badBatch=%v", probe.m.steps, probe.m.badBatch)
			}
			if enabled {
				if probe.m.batchCalls == 0 || probe.m.wheelCalls > 12 || probe.m.views > 20 {
					t.Fatalf("burst still computed a View per input: wheels=%d batches=%d views=%d", probe.m.wheelCalls, probe.m.batchCalls, probe.m.views)
				}
			} else if probe.m.batchCalls != 0 || probe.m.wheelCalls != 96 || probe.m.views < 96 {
				t.Fatalf("default delivery changed: wheels=%d batches=%d views=%d", probe.m.wheelCalls, probe.m.batchCalls, probe.m.views)
			}
			if tail := strings.Join(probe.m.seen[len(probe.m.seen)-2:], ","); tail != "key:k,key:q" {
				t.Fatalf("keys overtook wheel motion: %s", tail)
			}
		})
	}
}

func TestWheelCoalescingActualInputReversalDropsOnlyUncommittedMotion(t *testing.T) {
	probe := startWheelProbe(t, true, 4*time.Millisecond)
	probe.write(t, sgrWheel(64, 8, 4))
	select {
	case first := <-probe.m.wheels:
		if _, ok := first.(MouseWheelMsg); !ok {
			t.Fatalf("first wheel was not immediately delivered unchanged: %T", first)
		}
	case <-time.After(time.Second):
		t.Fatal("first wheel input was not responsive")
	}
	probe.write(t, strings.Repeat(sgrWheel(64, 8, 4), 60)+sgrWheel(65, 8, 4)+"q")
	probe.finish(t)
	t.Logf("first up + 60 uncommitted up + reverse down: wheel updates=%d net steps=%d", probe.m.wheelCalls, probe.m.steps)
	if probe.m.steps != 0 || probe.m.wheelCalls != 2 {
		t.Fatalf("old direction replayed after reversal: steps=%d wheel updates=%d", probe.m.steps, probe.m.wheelCalls)
	}
}

func TestWheelCoalescingActualInputKeepsKeysFocusHorizontalAndTargetsOrdered(t *testing.T) {
	probe := startWheelProbe(t, true, 2*time.Millisecond)
	up, down := sgrWheel(64, 8, 4), sgrWheel(65, 8, 4)
	probe.write(t, strings.Repeat(up, 12)+"k"+strings.Repeat(down, 8)+"\x1b[I"+
		sgrWheel(66, 8, 4)+sgrWheel(67, 8, 4)+sgrWheel(64, 10, 6)+sgrWheel(64+4, 10, 6)+"\x1b[Oq")
	probe.finish(t)
	want := make([]string, 0, 30)
	for i := 0; i < 12; i++ {
		want = append(want, fmt.Sprintf("wheel:%d:8:4:0", MouseWheelUp))
	}
	want = append(want, "key:k")
	for i := 0; i < 8; i++ {
		want = append(want, fmt.Sprintf("wheel:%d:8:4:0", MouseWheelDown))
	}
	want = append(want, "focus", fmt.Sprintf("wheel:%d:8:4:0", MouseWheelLeft), fmt.Sprintf("wheel:%d:8:4:0", MouseWheelRight),
		fmt.Sprintf("wheel:%d:10:6:0", MouseWheelUp), fmt.Sprintf("wheel:%d:10:6:%d", MouseWheelUp, ModShift), "blur", "key:q")
	if strings.Join(probe.m.seen, ",") != strings.Join(want, ",") {
		t.Fatalf("non-wheel boundary or pointer target reordered:\n got %v\nwant %v", probe.m.seen, want)
	}
}

func wheelBroker(t *testing.T, interval time.Duration) (chan<- Msg, <-chan Msg) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	input, output := make(chan Msg), make(chan Msg)
	done := make(chan struct{})
	p := NewProgram(&testModel{})
	go func() {
		defer close(done)
		p.coalesceMouseWheels(ctx, input, output, interval)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("wheel broker did not stop with a blocked output")
		}
	})
	return input, output
}

func receiveWheelMessage(t *testing.T, output <-chan Msg, limit time.Duration) Msg {
	t.Helper()
	select {
	case msg := <-output:
		return msg
	case <-time.After(limit):
		t.Fatal("wheel broker did not deliver its pending message")
		return nil
	}
}

func TestWheelCoalescingFirstEventImmediateAndWindowDoesNotSlide(t *testing.T) {
	input, output := wheelBroker(t, 300*time.Millisecond)
	wheel := MouseWheelMsg{Button: MouseWheelUp, X: 3, Y: 4}
	input <- wheel
	if msg := receiveWheelMessage(t, output, 100*time.Millisecond); msg != wheel {
		t.Fatalf("first event changed or waited for a frame: %#v", msg)
	}
	input <- wheel
	select {
	case msg := <-output:
		t.Fatalf("subsequent motion bypassed the frame boundary: %#v", msg)
	case <-time.After(100 * time.Millisecond):
	}
	input <- wheel
	// The window starts at the first delivery, not the most recent input.
	// A sliding debounce would not deliver before 400 ms from that first event.
	msg := receiveWheelMessage(t, output, 250*time.Millisecond)
	batch, ok := msg.(MouseWheelBatchMsg)
	if !ok || batch.Count != 2 || batch.MouseWheelMsg != wheel {
		t.Fatalf("unexpected frame-bounded batch: %#v", msg)
	}
}

func TestWheelCoalescingEveryNonWheelIsAnOrderingBarrier(t *testing.T) {
	for _, barrier := range []Msg{
		KeyPressMsg{Code: 'k', Text: "k"}, FocusMsg{}, BlurMsg{}, WindowSizeMsg{Width: 80, Height: 24},
		PasteMsg{Content: "approval"}, clearScreenMsg{}, printLineMessage{messageBody: "line"},
		struct{ Approval string }{Approval: "pending"},
		execMsg{}, BatchMsg(nil), sequenceMsg(nil), QuitMsg{},
		MouseClickMsg{Button: MouseLeft}, MouseReleaseMsg{Button: MouseLeft}, MouseMotionMsg{},
		MouseWheelMsg{Button: MouseWheelLeft}, MouseWheelMsg{Button: MouseWheelRight}, MouseWheelMsg{Button: MouseNone},
	} {
		t.Run(fmt.Sprintf("%T:%v", barrier, barrier), func(t *testing.T) {
			input, output := wheelBroker(t, time.Second)
			wheel := MouseWheelMsg{Button: MouseWheelUp, X: 3, Y: 4}
			input <- wheel
			receiveWheelMessage(t, output, time.Second)
			input <- wheel
			input <- wheel
			input <- barrier
			msg := receiveWheelMessage(t, output, 100*time.Millisecond)
			batch, ok := msg.(MouseWheelBatchMsg)
			if !ok || batch.Count != 2 {
				t.Fatalf("barrier did not immediately flush prior wheel motion: %#v", msg)
			}
			if msg := receiveWheelMessage(t, output, 100*time.Millisecond); !reflect.DeepEqual(msg, barrier) {
				t.Fatalf("non-wheel message changed or reordered: got %#v want %#v", msg, barrier)
			}
		})
	}
}

func TestWheelCoalescingBoundsExtremeMotionAndKeepsLatestDirection(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			input, output := wheelBroker(t, time.Second)
			wheel := MouseWheelMsg{Button: MouseWheelUp, X: 3, Y: 4}
			input <- wheel
			receiveWheelMessage(t, output, time.Second)
			for i := 0; i < 2000; i++ {
				input <- wheel
			}
			if reverse {
				wheel.Button = MouseWheelDown
				input <- wheel
			}
			input <- KeyPressMsg{Code: 'q', Text: "q"}
			msg := receiveWheelMessage(t, output, 100*time.Millisecond)
			batch, ok := msg.(MouseWheelBatchMsg)
			want := MaxMouseWheelBatchCount
			if reverse {
				want = 1
			}
			if !ok || batch.Count != want || batch.MouseWheelMsg != wheel {
				t.Fatalf("extreme burst accumulated stale or unbounded motion: %#v", msg)
			}
			if msg := receiveWheelMessage(t, output, 100*time.Millisecond); msg.(KeyPressMsg).Text != "q" {
				t.Fatalf("quit key did not follow bounded motion: %#v", msg)
			}
		})
	}
}

func TestWheelCoalescingFilterReceivesDocumentedOptInMessages(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			steps, raw, batches := 0, 0, 0
			filter := func(_ Model, msg Msg) Msg {
				switch msg := msg.(type) {
				case MouseWheelMsg:
					steps++
					raw++
				case MouseWheelBatchMsg:
					steps += msg.Count
					batches++
				}
				return msg
			}
			probe := startWheelProbe(t, enabled, 2*time.Millisecond, WithFilter(filter))
			probe.write(t, strings.Repeat(sgrWheel(64, 8, 4), 24)+"q")
			probe.finish(t)
			if steps != 24 || (!enabled && (raw != 24 || batches != 0)) || (enabled && batches == 0) {
				t.Fatalf("filter contract changed unexpectedly: steps=%d raw=%d batches=%d", steps, raw, batches)
			}
		})
	}
}
