package agent

import (
	"context"
	"testing"
	"testing/synctest"
)

// Regression: emit() used to drop events silently when the channel was
// full. Now it blocks until either the receiver catches up or ctx
// cancels — losing an EventPermissionRequest meant the sender's
// PermissionReply chan never got a decision and the tool call hung.

func TestEmit_BlocksWhenChannelFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan Event, 1)
		ch <- Event{Kind: EventInfo, Info: "first"} // fill it

		done := make(chan struct{})
		go func() {
			emit(context.Background(), ch, Event{Kind: EventInfo, Info: "second"})
			close(done)
		}()

		synctest.Wait() // emit goroutine must be durably blocked on send
		select {
		case <-done:
			t.Fatal("emit returned while channel was full — event would have been dropped")
		default:
			// Expected: emit is blocked on the channel.
		}

		<-ch            // make room
		synctest.Wait() // emit should now unblock and exit
		select {
		case <-done:
		default:
			t.Fatal("emit did not unblock after the channel was drained")
		}
	})
}

func TestEmit_CtxCancelUnblocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan Event, 1)
		ch <- Event{Kind: EventInfo, Info: "fill"}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			emit(ctx, ch, Event{Kind: EventInfo, Info: "blocked"})
			close(done)
		}()

		synctest.Wait() // emit blocked on send
		cancel()
		synctest.Wait() // emit should observe ctx.Done() and return
		select {
		case <-done:
		default:
			t.Fatal("emit did not return after ctx cancel")
		}
	})
}

func TestEmit_NilChannelIsNoOp(t *testing.T) {
	// Should not panic.
	emit(context.Background(), nil, Event{Kind: EventInfo})
}
