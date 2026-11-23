package tea

import (
	"context"
	"time"
)

// MaxMouseWheelBatchCount bounds the number of same-direction wheel steps in
// one MouseWheelBatchMsg. Extreme bursts within one frame saturate at this
// limit instead of creating an unbounded backlog of obsolete scrolling.
const MaxMouseWheelBatchCount = 256

const mouseWheelFrameInterval = 16 * time.Millisecond

// MouseWheelBatchMsg is emitted only with WithMouseWheelCoalescing. Count is
// positive and at most MaxMouseWheelBatchCount; Button carries the direction.
// Coordinates and modifiers are preserved, and only vertical wheel messages
// for the same coordinates and modifiers may be combined. Models opting in
// must handle Count without requesting a separate View for every step.
type MouseWheelBatchMsg struct {
	MouseWheelMsg
	Count int
}

// WithMouseWheelCoalescing combines bursts of decoded vertical wheel events
// before expensive model updates and views. The first wheel event is delivered
// immediately; subsequent steps are batched for at most about one 16 ms frame
// when the model is ready. All non-wheel messages remain ordering barriers.
// A direction reversal replaces uncommitted motion for the same target.
// Horizontal wheels and pointer-target changes are never combined.
// By default, every message retains Bubble Tea's original delivery behavior.
func WithMouseWheelCoalescing() ProgramOption {
	return func(p *Program) { p.mouseWheelCoalescing = true }
}

// coalesceMouseWheels sits on the shared message stream, not just the terminal
// reader. Command results, focus, approvals, clear/print/exec and all other
// non-wheel messages therefore flush prior motion before they are delivered.
// No model or renderer state is accessed on this goroutine.
func (p *Program) coalesceMouseWheels(ctx context.Context, input <-chan Msg, output chan<- Msg, interval time.Duration) {
	var timer *time.Timer
	var deadline <-chan time.Time
	var pending MouseWheelBatchMsg
	var last MouseWheelMsg
	var windowOpen, ready bool
	stopWindow := func() {
		if timer != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		deadline, windowOpen = nil, false
	}
	defer stopWindow()
	startWindow := func() {
		stopWindow()
		if timer == nil {
			timer = time.NewTimer(interval)
		} else {
			timer.Reset(interval)
		}
		deadline, windowOpen = timer.C, true
	}
	send := func(msg Msg) bool {
		select {
		case <-ctx.Done():
			return false
		case output <- msg:
			return true
		}
	}
	flushPending := func() bool {
		if pending.Count != 0 && !send(pending) {
			return false
		}
		pending, ready = MouseWheelBatchMsg{}, false
		return true
	}
	for {
		var destination chan<- Msg
		if ready && pending.Count != 0 {
			destination = output
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			deadline = nil
			if pending.Count == 0 {
				windowOpen = false
			} else {
				ready = true
			}
		case destination <- pending:
			pending, ready = MouseWheelBatchMsg{}, false
			startWindow()
		case msg, ok := <-input:
			if !ok {
				flushPending()
				return
			}
			wheel, vertical := p.translateInputEvent(msg).(MouseWheelMsg)
			vertical = vertical && (wheel.Button == MouseWheelUp || wheel.Button == MouseWheelDown)
			if !vertical {
				if !flushPending() {
					return
				}
				stopWindow()
				if !send(msg) {
					return
				}
				continue
			}
			sameTarget := last.X == wheel.X && last.Y == wheel.Y && last.Mod == wheel.Mod
			if !windowOpen || !sameTarget {
				if !flushPending() || !send(msg) {
					return
				}
				last = wheel
				startWindow()
				continue
			}
			if last.Button != wheel.Button {
				// Obsolete movement must not play back after the user reverses.
				// The latest direction is eligible for immediate delivery.
				pending = MouseWheelBatchMsg{MouseWheelMsg: wheel, Count: 1}
				ready = true
			} else if pending.Count == 0 {
				pending = MouseWheelBatchMsg{MouseWheelMsg: wheel, Count: 1}
			} else if pending.Count < MaxMouseWheelBatchCount {
				pending.Count++
			}
			last = wheel
		}
	}
}
