package tea

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type ctxImplodeMsg struct {
	cancel context.CancelFunc
}

type incrementMsg struct{}

type panicMsg struct{}

func panicCmd() Msg {
	panic("testing goroutine panic behavior")
}

type testModel struct {
	executed atomic.Value
	counter  atomic.Value
}

type controlledRendererTicker struct {
	ticks       chan time.Time
	stopEntered chan struct{}
	stopDone    chan struct{}
	allowStop   chan struct{}
	stopOnce    sync.Once
	releaseOnce sync.Once
	stopCount   atomic.Int32
}

func newControlledRendererTicker() *controlledRendererTicker {
	return &controlledRendererTicker{
		ticks:       make(chan time.Time, 2),
		stopEntered: make(chan struct{}),
		stopDone:    make(chan struct{}),
		allowStop:   make(chan struct{}),
	}
}

func (t *controlledRendererTicker) Ticks() <-chan time.Time {
	return t.ticks
}

func (t *controlledRendererTicker) Stop() {
	t.stopOnce.Do(func() {
		t.stopCount.Add(1)
		close(t.stopEntered)
		<-t.allowStop
		close(t.stopDone)
	})
}

func (t *controlledRendererTicker) releaseStop() {
	t.releaseOnce.Do(func() {
		close(t.allowStop)
	})
}

type rendererLifecycleProbe struct {
	renderer
	periodicFlushes chan struct{}
}

func (r *rendererLifecycleProbe) start() {}

func (r *rendererLifecycleProbe) close() error {
	return nil
}

func (r *rendererLifecycleProbe) flush(closing bool) error {
	if !closing {
		r.periodicFlushes <- struct{}{}
	}
	return nil
}

func waitForLifecycleSignal(t *testing.T, ch <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func (m *testModel) Init() Cmd {
	return nil
}

func (m *testModel) Update(msg Msg) (Model, Cmd) {
	switch msg := msg.(type) {
	case ctxImplodeMsg:
		msg.cancel()
		time.Sleep(100 * time.Millisecond)

	case incrementMsg:
		i := m.counter.Load()
		if i == nil {
			m.counter.Store(1)
		} else {
			m.counter.Store(i.(int) + 1)
		}

	case KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, Quit
		}

	case panicMsg:
		panic("testing panic behavior")
	}

	return m, nil
}

func (m *testModel) View() View {
	m.executed.Store(true)
	return NewView("success")
}

func TestRendererRestartKeepsTickerOwnershipPerGeneration(t *testing.T) {
	first := newControlledRendererTicker()
	second := newControlledRendererTicker()

	probe := &rendererLifecycleProbe{
		periodicFlushes: make(chan struct{}, 3),
	}
	p := NewProgram(&testModel{})
	p.renderer = probe
	t.Cleanup(func() {
		// Unblock either generation before asking an active renderer loop to
		// stop. Closing this test Program's done channel then broadcasts to
		// every possible generation, including regression implementations
		// that accidentally start more than one loop.
		first.releaseStop()
		second.releaseStop()
		close(p.rendererDone)
	})

	tickers := []rendererTicker{first, second}
	factoryCalls := 0
	p.newRendererTicker = func(time.Duration) rendererTicker {
		if factoryCalls >= len(tickers) {
			t.Fatalf("renderer ticker factory called more than %d times", len(tickers))
		}
		ticker := tickers[factoryCalls]
		factoryCalls++
		return ticker
	}

	p.startRenderer()
	first.ticks <- time.Now()
	waitForLifecycleSignal(t, probe.periodicFlushes, "first-generation flush")

	// The unbuffered rendererDone send returns as soon as generation one has
	// selected its teardown branch. Its Stop deliberately remains blocked so
	// generation two starts while the old teardown is still in flight.
	p.stopRenderer(false)
	waitForLifecycleSignal(t, first.stopEntered, "first-generation ticker stop")

	p.startRenderer()
	if factoryCalls != 2 {
		t.Fatalf("renderer restart reused an old ticker; factory calls = %d, want 2", factoryCalls)
	}
	second.ticks <- time.Now()
	waitForLifecycleSignal(t, probe.periodicFlushes, "second-generation flush")

	// Let the old teardown finish. It must only stop its own ticker; the new
	// renderer must continue flushing after that point.
	first.releaseStop()
	waitForLifecycleSignal(t, first.stopDone, "first-generation ticker shutdown")
	if got := second.stopCount.Load(); got != 0 {
		t.Fatalf("old renderer stopped the second-generation ticker %d times", got)
	}

	second.ticks <- time.Now()
	waitForLifecycleSignal(t, probe.periodicFlushes, "post-teardown second-generation flush")

	second.releaseStop()
	p.stopRenderer(true)
	waitForLifecycleSignal(t, second.stopDone, "second-generation ticker shutdown")

	if got := first.stopCount.Load(); got != 1 {
		t.Fatalf("first-generation ticker stop count = %d, want 1", got)
	}
	if got := second.stopCount.Load(); got != 1 {
		t.Fatalf("second-generation ticker stop count = %d, want 1", got)
	}
}

func TestTeaModel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var in bytes.Buffer
	in.Write([]byte("q"))

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	p := NewProgram(&testModel{},
		WithContext(ctx),
		WithInput(&in),
		WithOutput(&buf),
	)
	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}

	if buf.Len() == 0 {
		t.Fatal("no output")
	}
}

func TestTeaQuit(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var in bytes.Buffer

	m := &testModel{}
	p := NewProgram(m,
		WithInput(&in),
		WithOutput(&buf),
	)
	go func() {
		for {
			time.Sleep(time.Millisecond)
			if m.executed.Load() != nil {
				p.Quit()
				return
			}
		}
	}()

	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}
}

func TestTeaWaitQuit(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var in bytes.Buffer

	progStarted := make(chan struct{})
	waitStarted := make(chan struct{})
	errChan := make(chan error, 1)

	m := &testModel{}
	p := NewProgram(m,
		WithInput(&in),
		WithOutput(&buf),
	)

	go func() {
		_, err := p.Run()
		errChan <- err
	}()

	go func() {
		for {
			time.Sleep(time.Millisecond)
			if m.executed.Load() != nil {
				close(progStarted)

				<-waitStarted
				time.Sleep(50 * time.Millisecond)
				p.Quit()

				return
			}
		}
	}()

	<-progStarted

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			p.Wait()
			wg.Done()
		}()
	}
	close(waitStarted)
	wg.Wait()

	err := <-errChan
	if err != nil {
		t.Fatalf("Expected nil, got %v", err)
	}
}

func TestTeaWaitKill(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var in bytes.Buffer

	progStarted := make(chan struct{})
	waitStarted := make(chan struct{})
	errChan := make(chan error, 1)

	m := &testModel{}
	p := NewProgram(m,
		WithInput(&in),
		WithOutput(&buf),
	)

	go func() {
		_, err := p.Run()
		errChan <- err
	}()

	go func() {
		for {
			time.Sleep(time.Millisecond)
			if m.executed.Load() != nil {
				close(progStarted)

				<-waitStarted
				time.Sleep(50 * time.Millisecond)
				p.Kill()

				return
			}
		}
	}()

	<-progStarted

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			p.Wait()
			wg.Done()
		}()
	}
	close(waitStarted)
	wg.Wait()

	err := <-errChan
	if !errors.Is(err, ErrProgramKilled) {
		t.Fatalf("Expected %v, got %v", ErrProgramKilled, err)
	}
}

func TestTeaWithFilter(t *testing.T) {
	for _, preventCount := range []uint32{0, 1, 2} {
		t.Run(fmt.Sprintf("prevent_%d", preventCount), func(t *testing.T) {
			t.Parallel()
			testTeaWithFilter(t, preventCount)
		})
	}
}

func testTeaWithFilter(t *testing.T, preventCount uint32) {
	var buf bytes.Buffer
	var in bytes.Buffer

	m := &testModel{}
	shutdowns := uint32(0)
	p := NewProgram(m,
		WithInput(&in),
		WithOutput(&buf),
	)
	p.filter = func(_ Model, msg Msg) Msg {
		if _, ok := msg.(QuitMsg); !ok {
			return msg
		}
		if shutdowns < preventCount {
			atomic.AddUint32(&shutdowns, 1)
			return nil
		}
		return msg
	}

	go func() {
		for atomic.LoadUint32(&shutdowns) <= preventCount {
			time.Sleep(time.Millisecond)
			p.Quit()
		}
	}()

	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}
	if shutdowns != preventCount {
		t.Errorf("Expected %d prevented shutdowns, got %d", preventCount, shutdowns)
	}
}

func TestTeaKill(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var in bytes.Buffer

	m := &testModel{}
	p := NewProgram(m,
		WithInput(&in),
		WithOutput(&buf),
	)
	go func() {
		for {
			time.Sleep(time.Millisecond)
			if m.executed.Load() != nil {
				p.Kill()
				return
			}
		}
	}()

	_, err := p.Run()

	if !errors.Is(err, ErrProgramKilled) {
		t.Fatalf("Expected %v, got %v", ErrProgramKilled, err)
	}

	if errors.Is(err, context.Canceled) {
		// The end user should not know about the program's internal context state.
		// The program should only report external context cancellation as a context error.
		t.Fatalf("Internal context cancellation was reported as context error!")
	}
}

func TestTeaContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	var buf bytes.Buffer
	var in bytes.Buffer

	m := &testModel{}
	p := NewProgram(m,
		WithContext(ctx),
		WithInput(&in),
		WithOutput(&buf),
	)
	go func() {
		for {
			time.Sleep(time.Millisecond)
			if m.executed.Load() != nil {
				cancel()
				return
			}
		}
	}()

	_, err := p.Run()

	if !errors.Is(err, ErrProgramKilled) {
		t.Fatalf("Expected %v, got %v", ErrProgramKilled, err)
	}

	if !errors.Is(err, context.Canceled) {
		// The end user should know that their passed in context caused the kill.
		t.Fatalf("Expected %v, got %v", context.Canceled, err)
	}
}

func TestTeaContextImplodeDeadlock(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	var buf bytes.Buffer
	var in bytes.Buffer

	m := &testModel{}
	p := NewProgram(m,
		WithContext(ctx),
		WithInput(&in),
		WithOutput(&buf),
	)
	go func() {
		for {
			time.Sleep(time.Millisecond)
			if m.executed.Load() != nil {
				p.Send(ctxImplodeMsg{cancel: cancel})
				return
			}
		}
	}()

	if _, err := p.Run(); !errors.Is(err, ErrProgramKilled) {
		t.Fatalf("Expected %v, got %v", ErrProgramKilled, err)
	}
}

func TestTeaContextBatchDeadlock(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	var buf bytes.Buffer
	var in bytes.Buffer

	inc := func() Msg {
		cancel()
		return incrementMsg{}
	}

	m := &testModel{}
	p := NewProgram(m,
		WithContext(ctx),
		WithInput(&in),
		WithOutput(&buf),
	)
	go func() {
		for {
			time.Sleep(time.Millisecond)
			if m.executed.Load() != nil {
				batch := make(BatchMsg, 100)
				for i := range batch {
					batch[i] = inc
				}
				p.Send(batch)
				return
			}
		}
	}()

	if _, err := p.Run(); !errors.Is(err, ErrProgramKilled) {
		t.Fatalf("Expected %v, got %v", ErrProgramKilled, err)
	}
}

func TestTeaBatchMsg(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var in bytes.Buffer

	inc := func() Msg {
		return incrementMsg{}
	}

	m := &testModel{}
	p := NewProgram(m,
		WithInput(&in),
		WithOutput(&buf),
	)
	go func() {
		p.Send(BatchMsg{inc, inc})

		for {
			time.Sleep(time.Millisecond)
			i := m.counter.Load()
			if i != nil && i.(int) >= 2 {
				p.Quit()
				return
			}
		}
	}()

	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}

	if m.counter.Load() != 2 {
		t.Fatalf("counter should be 2, got %d", m.counter.Load())
	}
}

func TestTeaSequenceMsg(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var in bytes.Buffer

	inc := func() Msg {
		return incrementMsg{}
	}

	m := &testModel{}
	p := NewProgram(m,
		WithInput(&in),
		WithOutput(&buf),
	)
	go p.Send(sequenceMsg{inc, inc, Quit})

	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}

	if m.counter.Load() != 2 {
		t.Fatalf("counter should be 2, got %d", m.counter.Load())
	}
}

func TestTeaSequenceMsgWithBatchMsg(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var in bytes.Buffer

	inc := func() Msg {
		return incrementMsg{}
	}
	batch := func() Msg {
		return BatchMsg{inc, inc}
	}

	m := &testModel{}
	p := NewProgram(m,
		WithInput(&in),
		WithOutput(&buf),
	)
	go p.Send(sequenceMsg{batch, inc, Quit})

	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}

	if m.counter.Load() != 3 {
		t.Fatalf("counter should be 3, got %d", m.counter.Load())
	}
}

func TestTeaNestedSequenceMsg(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var in bytes.Buffer

	inc := func() Msg {
		return incrementMsg{}
	}

	m := &testModel{}
	p := NewProgram(m,
		WithInput(&in),
		WithOutput(&buf),
	)
	go p.Send(sequenceMsg{inc, Sequence(inc, inc, Batch(inc, inc)), Quit})

	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}

	if m.counter.Load() != 5 {
		t.Fatalf("counter should be 5, got %d", m.counter.Load())
	}
}

func TestTeaSend(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var in bytes.Buffer

	m := &testModel{}
	p := NewProgram(m,
		WithInput(&in),
		WithOutput(&buf),
	)

	// sending before the program is started is a blocking operation
	go p.Send(Quit())

	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}

	// sending a message after program has quit is a no-op
	p.Send(Quit())
}

func TestTeaNoRun(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var in bytes.Buffer

	m := &testModel{}
	NewProgram(m,
		WithInput(&in),
		WithOutput(&buf),
	)
}

func TestTeaPanic(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var in bytes.Buffer

	m := &testModel{}
	p := NewProgram(m,
		WithInput(&in),
		WithOutput(&buf),
	)
	go func() {
		for {
			time.Sleep(time.Millisecond)
			if m.executed.Load() != nil {
				p.Send(panicMsg{})
				return
			}
		}
	}()

	_, err := p.Run()

	if !errors.Is(err, ErrProgramPanic) {
		t.Fatalf("Expected %v, got %v", ErrProgramPanic, err)
	}

	if !errors.Is(err, ErrProgramKilled) {
		t.Fatalf("Expected %v, got %v", ErrProgramKilled, err)
	}
}

func TestTeaGoroutinePanic(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var in bytes.Buffer

	m := &testModel{}
	p := NewProgram(m,
		WithInput(&in),
		WithOutput(&buf),
	)
	go func() {
		for {
			time.Sleep(time.Millisecond)
			if m.executed.Load() != nil {
				batch := make(BatchMsg, 10)
				for i := 0; i < len(batch); i += 2 {
					batch[i] = Sequence(panicCmd)
					batch[i+1] = Batch(panicCmd)
				}
				p.Send(batch)
				return
			}
		}
	}()

	_, err := p.Run()

	if !errors.Is(err, ErrProgramPanic) {
		t.Fatalf("Expected %v, got %v", ErrProgramPanic, err)
	}

	if !errors.Is(err, ErrProgramKilled) {
		t.Fatalf("Expected %v, got %v", ErrProgramKilled, err)
	}
}

type benchModel struct {
	t testing.TB
}

func (m benchModel) Init() Cmd {
	return nil
}

func (m benchModel) Update(msg Msg) (Model, Cmd) {
	switch msg := msg.(type) {
	case KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, Quit
		}
	}

	return m, nil
}

func (m benchModel) View() View {
	view := strings.Join([]string{
		" \x1b[38;5;63m╭─────────────────────────╮\x1b[m",
		" \x1b[38;5;63m│\x1b[m\x1b[25X\x1b[28G\x1b[38;5;63m│\x1b[m",
		" \x1b[38;5;63m│\x1b[m    \x1b[38;5;231mHello There!\x1b[m    \x1b[38;5;63m│\x1b[m",
		" \x1b[38;5;63m│\x1b[m\x1b[25X\x1b[28G\x1b[38;5;63m│\x1b[m",
		" \x1b[38;5;63m╰─────────────────────────╯\x1b[m",
	}, "\n")

	return NewView(view)
}

func BenchmarkTeaRun(b *testing.B) {
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer

		m := benchModel{b}
		r, w := io.Pipe()
		p := NewProgram(m,
			WithInput(r),
			WithOutput(&buf),
		)

		go func() {
			for _, input := range "abcdefghijklmnopq" {
				time.Sleep(10 * time.Millisecond)
				w.Write([]byte(string(input)))
			}
		}()

		if _, err := p.Run(); err != nil {
			b.Fatalf("Run failed: %v", err)
		}

		_ = r.CloseWithError(io.EOF)
	}
}
