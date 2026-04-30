package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

// newClientWithStdio wires a Client around a stdio-shaped transport using
// pipes so we can drive the read loop from the test.
func newClientWithStdio(t *testing.T) (*Client, *io.PipeWriter, *failingWriter) {
	t.Helper()
	stdoutR, stdoutW := io.Pipe() // server → client (we write to stdoutW)
	failW := &failingWriter{}     // client → server (test inspects writes)

	tr := &StdioTransport{stdin: failW, stdout: stdoutR}
	c := NewClient(context.Background(), tr)
	go c.readLoop()
	return c, stdoutW, failW
}

// failingWriter is an io.WriteCloser that records writes and can be made
// to return errors on demand (simulates a dead subprocess pipe).
type failingWriter struct {
	mu      sync.Mutex
	written []byte
	failNow bool
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failNow {
		return 0, errors.New("pipe closed")
	}
	w.written = append(w.written, p...)
	return len(p), nil
}
func (w *failingWriter) Close() error { return nil }

// --- tests -----------------------------------------------------------------

func TestSend_GeneratesUniqueIDsUnderLoad(t *testing.T) {
	c := NewClient(context.Background(), nil)
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("%d", c.idSeq.Add(1))
		if seen[id] {
			t.Fatalf("duplicate id at iter %d: %s", i, id)
		}
		seen[id] = true
	}
}

func TestSend_StdioWriteErrorSurfacesImmediately(t *testing.T) {
	c, _, failW := newClientWithStdio(t)
	failW.mu.Lock()
	failW.failNow = true
	failW.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := c.send(ctx, "test/method", nil)
	if err == nil {
		t.Fatal("expected immediate error from failed write")
	}
	if ctx.Err() != nil {
		t.Errorf("ctx timed out instead of failing fast: %v", ctx.Err())
	}
}

func TestSend_PendingEntryDeletedOnSuccess(t *testing.T) {
	c, stdoutW, _ := newClientWithStdio(t)

	respDone := make(chan error, 1)
	go func() {
		_, err := c.send(context.Background(), "ping", nil)
		respDone <- err
	}()

	// Read the line the client wrote, parse the id, send a fake response back.
	var req JSONRPCRequest
	for {
		c.mu.RLock()
		n := len(c.pending)
		c.mu.RUnlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	c.mu.RLock()
	for id := range c.pending {
		req.ID = id
	}
	c.mu.RUnlock()

	resp := JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"ok":true}`)}
	b, _ := json.Marshal(resp)
	stdoutW.Write(append(b, '\n'))

	if err := <-respDone; err != nil {
		t.Fatalf("send returned err: %v", err)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.pending) != 0 {
		t.Errorf("pending should be empty after success, got %d", len(c.pending))
	}
}

func TestSend_PendingEntryDeletedOnCancel(t *testing.T) {
	c, _, _ := newClientWithStdio(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.send(ctx, "ping", nil)
		done <- err
	}()

	// Wait for send to register the pending entry, then cancel.
	for {
		c.mu.RLock()
		n := len(c.pending)
		c.mu.RUnlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	if err := <-done; err == nil {
		t.Fatal("expected error after cancel")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.pending) != 0 {
		t.Errorf("pending should be cleaned up after cancel, got %d entries", len(c.pending))
	}
}

func TestReadLoop_CancellingClientCtxWakesPendingSenders(t *testing.T) {
	c, stdoutW, _ := newClientWithStdio(t)

	done := make(chan error, 1)
	go func() {
		_, err := c.send(context.Background(), "ping", nil)
		done <- err
	}()

	// Wait for pending registration.
	for {
		c.mu.RLock()
		n := len(c.pending)
		c.mu.RUnlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// Simulate the subprocess dying: close server-side stdout. readLoop hits
	// EOF, calls c.cancel(), the sender wakes with ErrTransportClosed.
	stdoutW.Close()

	select {
	case err := <-done:
		if !errors.Is(err, ErrTransportClosed) {
			t.Errorf("expected ErrTransportClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not return after readLoop exited")
	}
}
