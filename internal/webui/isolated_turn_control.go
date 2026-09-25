package webui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/desktopipc"
)

// IsolatedSteerRequest asks the running worker to accept an additional user
// instruction. Reply must have capacity one; it reports the worker's actual
// acknowledgement rather than merely confirming a successful pipe write.
type IsolatedSteerRequest struct {
	Input string
	Reply chan bool
}

type isolatedWorkerControls struct {
	ctx      context.Context
	encoder  *desktopipc.Encoder
	failures chan<- error
	mu       sync.Mutex
	pending  map[string]chan bool
	closed   bool
	done     chan struct{}
}

func newIsolatedWorkerControls(ctx context.Context, encoder *desktopipc.Encoder, input <-chan IsolatedSteerRequest, failures chan<- error) *isolatedWorkerControls {
	controls := &isolatedWorkerControls{ctx: ctx, encoder: encoder, failures: failures, pending: make(map[string]chan bool), done: make(chan struct{})}
	go controls.run(input)
	return controls
}

func (c *isolatedWorkerControls) run(input <-chan IsolatedSteerRequest) {
	defer close(c.done)
	defer c.rejectPending()
	var next uint64
	for {
		select {
		case <-c.ctx.Done():
			return
		case request, ok := <-input:
			if !ok {
				// Closing the local control source should not interrupt a task
				// or discard acknowledgements already on their way back.
				input = nil
				continue
			}
			if request.Reply == nil || cap(request.Reply) < 1 {
				reportIsolatedReplyFailure(c.failures, errors.New("desktop worker steer requires a buffered reply channel"))
				return
			}
			if c.ctx.Err() != nil || strings.TrimSpace(request.Input) == "" {
				replyIsolatedSteer(request.Reply, false)
				continue
			}
			next++
			id := "steer-" + strconv.FormatUint(next, 10)
			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				replyIsolatedSteer(request.Reply, false)
				return
			}
			c.pending[id] = request.Reply
			c.mu.Unlock()
			if err := c.encoder.Encode(desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeSteer, ID: id, Input: request.Input}); err != nil {
				reportIsolatedReplyFailure(c.failures, fmt.Errorf("write desktop worker steer: %w", err))
				return
			}
		}
	}
}

func (c *isolatedWorkerControls) acknowledge(message desktopipc.Message) error {
	c.mu.Lock()
	reply, exists := c.pending[message.ID]
	if exists {
		delete(c.pending, message.ID)
	}
	c.mu.Unlock()
	if !exists {
		return errors.New("desktop worker steer acknowledgement references an unknown or resolved request")
	}
	if !replyIsolatedSteer(reply, message.Accepted) {
		return errors.New("desktop worker steer acknowledgement has no waiting reply slot")
	}
	return nil
}

func (c *isolatedWorkerControls) rejectPending() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for id, reply := range c.pending {
		replyIsolatedSteer(reply, false)
		delete(c.pending, id)
	}
}

func replyIsolatedSteer(reply chan bool, accepted bool) bool {
	select {
	case reply <- accepted:
		return true
	default:
		return false
	}
}
