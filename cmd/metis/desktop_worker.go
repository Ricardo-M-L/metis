package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"github.com/google/uuid"
)

type desktopWorkerPending struct {
	permission chan agent.PermissionDecision
	answer     chan string
}

// desktopWorkerBridge owns only presentation transport and pending decisions.
// The ordinary run path still owns permission policy, the loop, persistence,
// and cleanup. Losing the parent connection cancels the run; it never grants
// a permission or silently dismisses a live user's question.
type desktopWorkerBridge struct {
	ctx          context.Context
	cancel       context.CancelCauseFunc
	input        io.ReadCloser
	encoder      *desktopipc.Encoder
	mu           sync.Mutex
	pending      map[string]desktopWorkerPending
	err          error
	closed       bool
	done         chan struct{}
	steerHandler func(string) bool
	steers       chan desktopipc.Message
	steerDone    chan struct{}
}

func newDesktopWorkerBridge(parent context.Context, input io.ReadCloser, output io.Writer) *desktopWorkerBridge {
	ctx, cancel := context.WithCancelCause(parent)
	b := &desktopWorkerBridge{ctx: ctx, cancel: cancel, input: input, encoder: desktopipc.NewEncoder(output), pending: make(map[string]desktopWorkerPending), done: make(chan struct{}), steers: make(chan desktopipc.Message, 16), steerDone: make(chan struct{})}
	go b.readReplies()
	go b.handleSteers()
	return b
}

func (b *desktopWorkerBridge) readReplies() {
	defer close(b.done)
	decoder := desktopipc.NewDecoder(b.input)
	for {
		message, err := decoder.Decode()
		if err != nil {
			b.fail(fmt.Errorf("desktop worker reply stream: %w", err))
			return
		}
		if message.Type == desktopipc.TypeSteer {
			select {
			case b.steers <- message:
			case <-b.ctx.Done():
				return
			default:
				b.fail(errors.New("too many pending desktop worker steer requests"))
				return
			}
			continue
		}
		if message.Type != desktopipc.TypeReply {
			b.fail(errors.New("desktop worker expected a reply"))
			return
		}
		b.mu.Lock()
		pending, ok := b.pending[message.ID]
		if ok {
			delete(b.pending, message.ID)
		}
		b.mu.Unlock()
		if !ok {
			b.fail(errors.New("desktop worker reply references an unknown or resolved request"))
			return
		}
		if pending.permission != nil {
			select {
			case pending.permission <- message.Decision:
			case <-b.ctx.Done():
				return
			}
		} else {
			select {
			case pending.answer <- message.Answer:
			case <-b.ctx.Done():
				return
			}
		}
	}
}

func (b *desktopWorkerBridge) setSteerHandler(handler func(string) bool) {
	b.mu.Lock()
	b.steerHandler = handler
	b.mu.Unlock()
}

// A compaction can briefly hold Loop's history lock. Keep that wait off the
// input decoder so unrelated tool approvals are still delivered immediately.
func (b *desktopWorkerBridge) handleSteers() {
	defer close(b.steerDone)
	for {
		select {
		case <-b.ctx.Done():
			return
		case message := <-b.steers:
			if b.ctx.Err() != nil {
				return
			}
			b.mu.Lock()
			handler := b.steerHandler
			b.mu.Unlock()
			accepted := handler != nil && handler(message.Input)
			if err := b.encoder.Encode(desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeSteerResult, ID: message.ID, Accepted: accepted}); err != nil {
				b.fail(err)
				return
			}
		}
	}
}

func (b *desktopWorkerBridge) emit(event agent.Event) error {
	wire := desktopipc.FromEvent(event)
	message := desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeEvent, Event: &wire}
	pending := desktopWorkerPending{}
	switch event.Kind {
	case agent.EventPermissionRequest:
		if event.PermissionReply == nil {
			return b.fail(errors.New("desktop worker permission request has no reply channel"))
		}
		pending.permission = event.PermissionReply
	case agent.EventAskUser:
		if event.AskUserReply == nil {
			return b.fail(errors.New("desktop worker question has no reply channel"))
		}
		pending.answer = event.AskUserReply
	}
	if pending.permission != nil || pending.answer != nil {
		message.ID = uuid.NewString()
		b.mu.Lock()
		if b.err != nil || b.closed || b.ctx.Err() != nil {
			b.mu.Unlock()
			return context.Cause(b.ctx)
		}
		b.pending[message.ID] = pending
		b.mu.Unlock()
	}
	if err := b.encoder.Encode(message); err != nil {
		return b.fail(fmt.Errorf("desktop worker event stream: %w", err))
	}
	return nil
}

func (b *desktopWorkerBridge) fail(err error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed && b.err == nil {
		b.err = err
		b.cancel(err)
		// Cancellation makes Loop's select stop waiting. Drop references without
		// sending synthetic allow/answer decisions or blocking on a full channel.
		clear(b.pending)
	}
	return err
}

func (b *desktopWorkerBridge) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

func (b *desktopWorkerBridge) Close() {
	b.mu.Lock()
	b.closed = true
	clear(b.pending)
	b.mu.Unlock()
	b.cancel(context.Canceled)
	// Unix workers use a pollable duplicated stdin and join immediately. Some
	// hosts cannot interrupt an inherited synchronous pipe read; this private
	// one-shot process must still be able to exit after its runtime is durable.
	go func() { _ = b.input.Close() }()
	select {
	case <-b.done:
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case <-b.steerDone:
	case <-time.After(100 * time.Millisecond):
	}
}
