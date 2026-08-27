package agent

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
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

func TestEmitTokens_CanceledContextPrefersWritableChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A plain select between a writable send and an already-closed Done
	// channel chooses pseudo-randomly. Repeating the canceled-at-entry case
	// makes that old behavior fail reliably while the required send-first
	// policy remains deterministic.
	for i := 0; i < 512; i++ {
		ch := make(chan Event, 1)
		want := Event{Kind: EventTokens, InputTokens: i + 1}
		emit(ctx, ch, want)
		select {
		case got := <-ch:
			if got.Kind != want.Kind || got.InputTokens != want.InputTokens {
				t.Fatalf("iteration %d: event = %+v, want %+v", i, got, want)
			}
		default:
			t.Fatalf("iteration %d: canceled context dropped token usage despite writable channel", i)
		}
	}
}

func TestEmitTokens_NormalContextBlocksUntilFullChannelDrains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan Event, 1)
		ch <- Event{Kind: EventInfo, Info: "fill"}

		done := make(chan struct{})
		go func() {
			emit(context.Background(), ch, Event{Kind: EventTokens, InputTokens: 42})
			close(done)
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("token usage emit returned while a live context's channel was full")
		default:
		}

		<-ch
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("token usage emit did not unblock after channel drain")
		}
		if got := <-ch; got.Kind != EventTokens || got.InputTokens != 42 {
			t.Fatalf("delivered event = %+v, want token usage", got)
		}
	})
}

func TestEmitTokens_CanceledContextDoesNotBlockOnFullChannelAndStillTraces(t *testing.T) {
	ch := make(chan Event, 1)
	ch <- Event{Kind: EventInfo, Info: "fill"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	traced := make(chan Event, 1)
	SetTraceHook(func(ev Event) { traced <- ev })
	t.Cleanup(func() { SetTraceHook(nil) })

	done := make(chan struct{})
	go func() {
		emit(ctx, ch, Event{Kind: EventTokens, InputTokens: 99})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled token usage emit blocked on a full channel")
	}
	if got := <-ch; got.Kind != EventInfo || got.Info != "fill" {
		t.Fatalf("full channel contents changed: %+v", got)
	}
	select {
	case got := <-traced:
		if got.Kind != EventTokens || got.InputTokens != 99 {
			t.Fatalf("trace event = %+v, want token usage", got)
		}
	default:
		t.Fatal("canceled token usage was not forwarded to the trace hook")
	}
}

func TestEmitStampsInternalTraceInvocationSeparatelyFromPublicParentID(t *testing.T) {
	ctx := WithParentToolUseID(context.Background(), "provider-public-id")
	ctx = WithTraceInvocationID(ctx, "process-unique-id")
	out := make(chan Event, 1)
	emit(ctx, out, Event{Kind: EventTextDelta, TextDelta: "child"})

	got := <-out
	if got.SubAgentParentID != "provider-public-id" {
		t.Fatalf("public parent id = %q", got.SubAgentParentID)
	}
	if got.TraceInvocationID != "process-unique-id" {
		t.Fatalf("internal trace invocation id = %q", got.TraceInvocationID)
	}
}

func TestNewTraceInvocationIDIsProcessUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1024)
	for i := 0; i < 1024; i++ {
		id := NewTraceInvocationID()
		if id == "" {
			t.Fatal("empty trace invocation id")
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate trace invocation id %q", id)
		}
		seen[id] = struct{}{}
	}
}
